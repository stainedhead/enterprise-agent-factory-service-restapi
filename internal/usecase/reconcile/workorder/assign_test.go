package workorder_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

func TestAssignerCreatesTeamsAndBacksOffAFailingWorkOrder(t *testing.T) {
	now := t0
	st := &fakeAssignStore{ids: []string{"wo_a", "wo_b"}, failures: map[string]error{"wo_a": errors.New("swarm template is disabled")}}
	a := &workorder.Assigner{Store: st, Now: func() time.Time { return now }}

	if err := a.Run(t.Context()); err == nil {
		t.Fatal("the failure of wo_a must be reported")
	}
	if st.teams["wo_b"] == "" || st.calls["wo_a"] != 1 || st.calls["wo_b"] != 1 {
		t.Fatalf("teams = %v, calls = %v: a failing work order must not stop the others", st.teams, st.calls)
	}

	// Within the delay wo_a is not tried again; after it, it is, with a longer delay each time.
	now = now.Add(10 * time.Second)
	if err := a.Run(t.Context()); err != nil || st.calls["wo_a"] != 1 {
		t.Fatalf("retried too early: err = %v, calls = %d", err, st.calls["wo_a"])
	}
	now = now.Add(25 * time.Second)
	_ = a.Run(t.Context())
	if st.calls["wo_a"] != 2 {
		t.Fatalf("not retried after the delay: %d", st.calls["wo_a"])
	}
	now = now.Add(45 * time.Second) // the second delay is 60 s
	_ = a.Run(t.Context())
	if st.calls["wo_a"] != 2 {
		t.Fatalf("second delay too short: %d", st.calls["wo_a"])
	}
	now = now.Add(20 * time.Second)
	delete(st.failures, "wo_a") // the template was enabled
	if err := a.Run(t.Context()); err != nil || st.teams["wo_a"] != "team_for_wo_a" {
		t.Fatalf("recovered: err = %v, teams = %v", err, st.teams)
	}
}

func TestAssignerProvisionsTemplateSecrets(t *testing.T) {
	ss := &fakeSecretStore{pending: []workorder.TemplateSecret{
		{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_1", Pool: "triage", Name: "scanner-token", Path: "afs/afs-t-wo-x/triage/scanner-token"},
		{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_1", Pool: "triage", Name: "Bad Name", Path: "p2"},
		{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_1", Pool: "triage", Name: "scanner-token", Path: "dup"},
		{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_2", Pool: "fixers", Name: "deploy-key", Delivery: "file", Target: "/etc/afs/key", Path: "afs/afs-t-wo-x/fixers/deploy-key"},
	}}
	cr := &fakeCreator{}
	a := &workorder.Assigner{Store: &fakeAssignStore{}, Now: clock, Secrets: &workorder.Secrets{Store: ss, Creator: cr}}
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(cr.created) != 2 || len(ss.recorded) != 2 {
		t.Fatalf("created %v recorded %d: the invalid and the duplicate entries are skipped", cr.created, len(ss.recorded))
	}
	if ss.recorded[0].Delivery != "env" || ss.recorded[0].Target != "SCANNER_TOKEN" {
		t.Errorf("defaults not applied: %+v", ss.recorded[0])
	}
	if cr.tags[0][workorder.TagTeamID] != "team_a" || cr.tags[0][workorder.TagPoolID] != "pool_1" || cr.tags[0][workorder.TagWorkOrderID] != "wo_a" {
		t.Errorf("tags = %v: the team tag is what teardown finds the secret by", cr.tags[0])
	}
}

func TestSecretsWaitsForTheDatabaseFunction(t *testing.T) {
	ss := &fakeSecretStore{unsupported: true, pending: []workorder.TemplateSecret{{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_1", Name: "x-token", Path: "p"}}}
	cr := &fakeCreator{}
	s := &workorder.Secrets{Store: ss, Creator: cr}
	for i := 0; i < 2; i++ {
		if err := s.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(cr.created) != 0 || len(ss.recorded) != 0 {
		t.Errorf("created %v: without the -db function nothing may be created (it would be an orphan)", cr.created)
	}
}

func TestSecretsFailuresAreRetriedAndReported(t *testing.T) {
	p := []workorder.TemplateSecret{{WorkOrderID: "wo_a", TeamID: "team_a", PoolID: "pool_1", Name: "x-token", Path: "p"}}
	cr := &fakeCreator{err: errors.New("throttled")}
	if err := (&workorder.Secrets{Store: &fakeSecretStore{pending: p}, Creator: cr}).Run(t.Context()); err == nil {
		t.Error("a create failure must be reported")
	}
	ss := &fakeSecretStore{pending: p, recordErr: errors.New("db down")}
	if err := (&workorder.Secrets{Store: ss, Creator: &fakeCreator{}}).Run(t.Context()); err == nil {
		t.Error("a record failure must be reported")
	}
	// Every case: the entry stays pending (Pending is derived from the table), so the next pass retries.
}

func TestTemplateSecretNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   workorder.TemplateSecret
		ok   bool
	}{
		{"env default", workorder.TemplateSecret{Name: "api-key"}, true},
		{"file", workorder.TemplateSecret{Name: "k", Delivery: "file", Target: "/etc/afs/k"}, true},
		{"file relative", workorder.TemplateSecret{Name: "k", Delivery: "file", Target: "etc/k"}, false},
		{"env bad target", workorder.TemplateSecret{Name: "k", Delivery: "env", Target: "lower"}, false},
		{"bad delivery", workorder.TemplateSecret{Name: "k", Delivery: "volume", Target: "X"}, false},
		{"bad name", workorder.TemplateSecret{Name: "9lives"}, false},
	}
	for _, tc := range cases {
		if why := tc.in.Normalize(); (why == "") != tc.ok {
			t.Errorf("%s: %q", tc.name, why)
		}
	}
}
