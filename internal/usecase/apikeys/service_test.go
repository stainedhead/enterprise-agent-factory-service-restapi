package apikeys_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys/apikeystest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func rig() (*apikeys.Service, *apikeystest.Repo, *fakes.Audit, *fakes.Outbox, *fakes.Idempotency) {
	repo, audit, outbox, idem := apikeystest.New(), &fakes.Audit{}, &fakes.Outbox{}, &fakes.Idempotency{}
	return &apikeys.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: outbox, Idem: idem,
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: repo, Entropy: bytes.NewReader(bytes.Repeat([]byte{9}, 1024))}, repo, audit, outbox, idem
}

var admin = acting.Caller{Principal: authz.Principal{ID: "usr_c", Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}}

func params(name string) domain.NewAPIKeyParams {
	return domain.NewAPIKeyParams{Name: name, Role: domain.TeamRoleTeamViewer, ExpiresAt: now.Add(30 * 24 * time.Hour)}
}

func render(v any) (int, json.RawMessage, error) {
	c := v.(apikeys.Created)
	b, err := json.Marshal(map[string]any{"id": c.Key.ID, "name": c.Key.Name, "secretKey": c.Secret})
	return 201, b, err
}

func TestCreateShowsSecretOnceAndStoresHash(t *testing.T) {
	svc, repo, audit, outbox, _ := rig()
	res, err := svc.Create(t.Context(), admin, team, params("ci"), nil)
	if err != nil || !strings.HasPrefix(res.Secret, "afsk_") {
		t.Fatalf("%+v %v", res, err)
	}
	if repo.Hashes[res.Key.ID] != domain.HashAPIKey(res.Secret) {
		t.Fatal("stored hash is not the SHA-256 of the secret")
	}
	if len(audit.Entries) != 1 || len(outbox.Events) != 0 {
		t.Fatalf("audit=%d outbox=%d", len(audit.Entries), len(outbox.Events))
	}
	if b, _ := json.Marshal(audit.Entries[0].Record); strings.Contains(string(b), res.Secret) {
		t.Fatal("secret in audit record")
	}
	if _, err := svc.Create(t.Context(), admin, team, params("ci"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate name: %v", err)
	}
	bad := params("x")
	bad.ExpiresAt = now.Add(91 * 24 * time.Hour)
	if _, err := svc.Create(t.Context(), admin, team, bad, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("91 days: %v", err)
	}
	// entropy exhausted
	svc.Entropy = bytes.NewReader(nil)
	if _, err := svc.Create(t.Context(), admin, team, params("z"), nil); err == nil {
		t.Fatal("want entropy error")
	}
}

func TestReplayReturnsNoSecret(t *testing.T) {
	svc, repo, audit, _, store := rig()
	mk := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "CreateApiKey", RequestSHA256: [32]byte{1}}, Render: render}
	}
	first, err := svc.Create(t.Context(), admin, team, params("ci"), mk())
	if err != nil || first.Secret == "" {
		t.Fatal(err)
	}
	again, err := svc.Create(t.Context(), admin, team, params("ci"), mk())
	if err != nil || again.Replay == nil || again.Secret != "" {
		t.Fatalf("%+v %v", again, err)
	}
	if strings.Contains(string(again.Replay.Body), "secretKey") || strings.Contains(string(again.Replay.Body), first.Secret) {
		t.Fatalf("replayed body holds the secret: %s", again.Replay.Body)
	}
	if len(repo.Keys) != 1 || len(audit.Entries) != 1 {
		t.Fatal("replay wrote")
	}
	_ = store
}

func TestListAndDelete(t *testing.T) {
	svc, repo, audit, _, _ := rig()
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		r, err := svc.Create(t.Context(), admin, team, params(n), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Key.ID)
	}
	rows, next, err := svc.List(t.Context(), admin, team, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("%v %q %v", rows, next, err)
	}
	rows, next, _ = svc.List(t.Context(), admin, team, acting.Page{Limit: 2, Cursor: next})
	if len(rows) != 1 || next != "" {
		t.Fatalf("page 2: %v %q", rows, next)
	}
	if err := svc.Delete(t.Context(), admin, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002", ids[0]); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
	if err := svc.Delete(t.Context(), admin, team, ids[0]); err != nil {
		t.Fatal(err)
	}
	if len(repo.Keys) != 2 || len(audit.Entries) != 4 {
		t.Fatalf("keys=%d audit=%d", len(repo.Keys), len(audit.Entries))
	}
}
