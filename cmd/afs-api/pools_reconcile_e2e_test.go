package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool/pooltest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

// TestPoolChangesThroughTheAPIRerenderThePodTemplate joins the two M5 streams: a change made through the
// API (M5-B: a config value, handoffs) reaches the pool controller (M5-C), which reads the pool as the
// reconciler does and renders a new pod template for it; a handoff change raises agent_pools.generation.
// Scaling changes replicas only, so the pod template hash stays.
func TestPoolChangesThroughTheAPIRerenderThePodTemplate(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := wire(context.Background(), quiet, settings{
		DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	call := func(method, path, body string, hdr ...string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+w.Tokens[matrix.ServiceAdmin])
		r.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		c.Handler.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	must := func(what string, got, want int, b map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: %d, want %d: %v", what, got, want, b)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}

	soon := time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	st, b := call(http.MethodPost, "/v1/teams", fmt.Sprintf(`{"slug":"sw-rr-%s","displayName":"Swarm","costCenter":"CC1","ownerEmail":"owner@example.com","maxBudgetUsd":"1000","kind":"ephemeral","firstAdmin":{"subjectType":"group","groupName":"rr-g"},"tags":[],"expiresAt":%q,"teardownRule":"at_expiry","budget":{"amountUsd":"50","period":"lifetime"}}`,
		strings.ToLower(pgtest.NewID("x")[2:8]), soon))
	must("create ephemeral team", st, http.StatusCreated, b)
	swarm, _ := b["id"].(string)
	model, tpl := "rr.model-"+strings.ToLower(pgtest.NewID("x")[2:10]), "rr-"+strings.ToLower(pgtest.NewID("x")[2:10])
	exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'RR', 4096)`, model)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2)`, swarm, model)
	exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, pgtest.NewID("tpl"), tpl, poolDigest)
	mk := func(name string) string {
		st, b := call(http.MethodPost, "/v1/teams/"+swarm+"/pools",
			fmt.Sprintf(`{"name":%q,"template":%q,"image":%q,"modelId":%q,"workers":2}`, name, tpl, "registry.example.com/agents/coder@"+poolDigest, model))
		must("create pool "+name, st, http.StatusCreated, b)
		id, _ := b["id"].(string)
		return id
	}
	sender, receiver := mk("sender"), mk("receiver")
	one := "/v1/teams/" + swarm + "/pools/" + sender

	// The reconciler's side: the real database source and controller, fakes for AWS and the cluster.
	rpool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpool.Close)
	cl := &pooltest.Cluster{State: pool.Workers{Exists: true, Desired: 2, Ready: 2}}
	ctl, err := pool.NewController(pool.Config{
		Source: postgres.NewPoolSource(postgres.NewTxRunner(rpool)), Cluster: cl, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities(),
		Queues: pooltest.NewQueues(), Recorder: &pooltest.Recorder{}, Probe: &pooltest.Probe{Fail: map[string]error{}},
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", SecretsPrefix: "afs",
			GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", PermissionsBoundaryARN: "arn:aws:iam::111122223333:policy/afs-boundary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := func() int64 {
		t.Helper()
		var g int64
		if err := w.Admin.QueryRow(t.Context(), `SELECT generation FROM afs.agent_pools WHERE id = $1`, sender).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	render := func() pool.Environment {
		t.Helper()
		out, err := ctl.Reconcile(t.Context(), engine.NewRef(engine.KindPool, sender, swarm))
		if err != nil {
			t.Fatal(err)
		}
		if out.Generation != generation() {
			t.Fatalf("the controller observed generation %d, the row is at %d", out.Generation, generation())
		}
		return cl.Last()
	}

	base := render()
	if base.Hash == "" || base.Replicas != 2 {
		t.Fatalf("first render = %+v", base)
	}

	// A config value changes the pod template.
	st, b = call(http.MethodPut, one+"/config-values/LOG_LEVEL", `{"source":"literal","value":"debug"}`)
	must("put config value", st, http.StatusOK, b)
	// (pool_config_values has the database's revision and change-feed triggers, so the reconciler hears
	// of it through the feed's pool_config_values route; handoffs and secret bindings have no trigger and
	// raise the pool's generation instead.)
	cfg := render()
	if cfg.Hash == base.Hash {
		t.Fatal("a config value change did not change the pod template hash")
	}

	// A handoff changes it again (the pool now sends to the receiver's inbox).
	g1 := generation()
	st, b = call(http.MethodPut, one+"/handoffs", `{"toPoolIds":["`+receiver+`"]}`)
	must("put handoffs", st, http.StatusOK, b)
	if generation() <= g1 {
		t.Fatalf("generation = %d after handoffs, want above %d", generation(), g1)
	}
	hand := render()
	if hand.Hash == cfg.Hash {
		t.Fatal("a handoff did not change the pod template hash")
	}

	// Scaling changes the replicas and nothing in the template.
	st, b = call(http.MethodPost, one+":scale", `{"workers":5}`)
	must("scale", st, http.StatusOK, b)
	scaled := render()
	if scaled.Replicas != 5 || scaled.Hash != hand.Hash {
		t.Fatalf("scale: replicas %d, hash changed %t", scaled.Replicas, scaled.Hash != hand.Hash)
	}

	// Removing the config value takes it out of the template.
	var cv int
	if err := w.Admin.QueryRow(t.Context(), `SELECT version FROM afs.pool_config_values WHERE pool_id = $1 AND name = 'LOG_LEVEL'`, sender).Scan(&cv); err != nil {
		t.Fatal(err)
	}
	st, b = call(http.MethodDelete, one+"/config-values/LOG_LEVEL", "", "If-Match", fmt.Sprintf(`"%d"`, cv))
	must("delete config value", st, http.StatusNoContent, b)
	if after := render(); after.Hash == hand.Hash {
		t.Fatal("deleting the config value did not change the pod template hash")
	}

	// Delete: the API answers 202 with state deleting; the reconciler's finalizer writes state deleted
	// and deleted_at (X-DB-5), after which the API no longer finds the pool. The status kind both sides
	// use is "pool".
	if pools.StatusKind != string(engine.KindPool) {
		t.Fatalf("the API reads pool status under %q, the reconciler writes %q", pools.StatusKind, engine.KindPool)
	}
	var pv int
	if err := w.Admin.QueryRow(t.Context(), `SELECT version FROM afs.agent_pools WHERE id = $1`, sender).Scan(&pv); err != nil {
		t.Fatal(err)
	}
	st, b = call(http.MethodDelete, one, "", "If-Match", fmt.Sprintf(`"%d"`, pv))
	must("delete pool", st, http.StatusAccepted, b)
	if b["state"] != "deleting" {
		t.Fatalf("delete body = %v", b)
	}
	out, err := ctl.Reconcile(t.Context(), engine.NewRef(engine.KindPool, sender, swarm))
	if err != nil || !out.NoStatus {
		t.Fatalf("a deleting pool without a finalizer hook: %+v, %v", out, err)
	}
	lc := postgres.NewPoolLifecycle(postgres.NewTxRunner(rpool), postgres.NewAuditWriter(postgres.NewTxRunner(rpool)), postgres.NewOutboxWriter(), &fakes.IDGen{})
	if err := lc.Complete(t.Context(), lifecycle.Target{Kind: "pool", ID: sender, TeamID: swarm}); err != nil {
		t.Fatal(err)
	}
	st, b = call(http.MethodGet, one, "")
	must("a finalized pool", st, http.StatusNotFound, b)
}
