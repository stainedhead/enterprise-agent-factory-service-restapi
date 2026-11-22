package imports_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports/importstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var admin = acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}

type rig struct {
	svc    *imports.Service
	repo   *importstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: importstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &imports.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	return r
}

func team(slug string, agents int, secretARN string) domain.TeamBundle {
	tb := domain.TeamBundle{Team: domain.Row{"slug": slug, "allowed_aws_accounts": []any{}}, Secrets: []domain.Row{}}
	if secretARN != "" {
		tb.Secrets = []domain.Row{{"name": "token", "secrets_manager_arn": secretARN}}
	}
	for range agents {
		tb.Agents = append(tb.Agents, domain.AgentBundle{Agent: domain.Row{"name": "coder"}})
	}
	return tb
}

func doc(t *testing.T, teams ...domain.TeamBundle) json.RawMessage {
	t.Helper()
	_, d, err := domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: "2026-11-22T00:00:00Z",
		ExternalSystems: []domain.Row{}, Teams: teams}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApplyReportsPerTeamAndWritesAuditAndEvents(t *testing.T) {
	r := newRig()
	r.repo.Existing["old"] = true
	r.repo.Existing["same"] = true
	res, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t, team("new", 0, ""), team("old", 1, ""), team("same", 0, "")), DryRun: false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tr := range res.Report.Teams {
		got[tr.Slug] = tr.Result
	}
	if got["new"] != imports.ResultCreated || got["old"] != imports.ResultUpdated || got["same"] != imports.ResultSkipped || res.Report.DryRun {
		t.Fatalf("%+v", res.Report)
	}
	if !strings.Contains(strings.Join(res.Report.Teams[0].Changes, "|"), "value must be put again") {
		t.Fatalf("secrets needing values are not listed: %v", res.Report.Teams[0].Changes)
	}
	// one audit per resource (2) plus the import itself; events: team.created, team.updated, import.applied
	if len(r.audit.Entries) != 3 || len(r.outbox.Events) != 3 || r.outbox.Events[2].Type != domain.EventImportApplied {
		t.Fatalf("audit %d events %d", len(r.audit.Entries), len(r.outbox.Events))
	}
	if len(r.repo.Records) != 1 || r.repo.Records[0].DryRun || r.repo.Records[0].Failed {
		t.Fatalf("%+v", r.repo.Records)
	}
}

func TestDryRunWritesNothingToDesiredState(t *testing.T) {
	r := newRig()
	res, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t, team("new", 0, "")), DryRun: true}, nil)
	if err != nil || !res.Report.DryRun || res.Report.Teams[0].Result != imports.ResultCreated {
		t.Fatalf("%+v %v", res, err)
	}
	for _, e := range r.audit.Entries {
		if e.Record.Action == "ImportResource" {
			t.Fatalf("a dry run kept a resource audit record: %+v", e)
		}
	}
	for _, ev := range r.outbox.Events {
		t.Fatalf("a dry run published %s", ev.Type)
	}
	if len(r.repo.Records) != 1 || !r.repo.Records[0].DryRun {
		t.Fatalf("the report row: %+v", r.repo.Records)
	}
}

func TestFailedTeamIsReportedAndOthersApplied(t *testing.T) {
	r := newRig()
	r.repo.FailSlug = "bad"
	res, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t, team("bad", 1, ""), team("good", 0, ""))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bad, good := res.Report.Teams[0], res.Report.Teams[1]
	if bad.Result != imports.ResultFailed || bad.Error == nil || len(bad.Changes) != 0 || good.Result != imports.ResultCreated {
		t.Fatalf("%+v %+v", bad, good)
	}
	for _, e := range r.audit.Entries {
		if e.Record.TeamID == "team_bad" {
			t.Fatal("a failed team left an audit record")
		}
	}
	if !r.repo.Records[0].Failed {
		t.Fatal("the import row does not show the failure")
	}
}

func TestAccountMapRules(t *testing.T) {
	r := newRig()
	arn := "arn:aws:secretsmanager:eu-west-1:111122223333:secret:afs/p/_shared/token-AbCdEf"
	res, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t, team("p", 0, arn)),
		AccountMap: map[string]string{"444455556666": "000011112222"}}, nil)
	if err != nil || res.Report.Teams[0].Result != imports.ResultFailed || !strings.Contains(*res.Report.Teams[0].Error, "111122223333") {
		t.Fatalf("a missing account must fail the team: %+v %v", res.Report, err)
	}
	if len(r.repo.Applied) != 0 {
		t.Fatal("the team was applied")
	}
	res, err = r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t, team("p", 0, arn)),
		AccountMap: map[string]string{"111122223333": "000011112222"}}, nil)
	if err != nil || res.Report.Teams[0].Result != imports.ResultCreated {
		t.Fatalf("%+v %v", res.Report, err)
	}
	if got := r.repo.Applied[0].Secrets[0]["secrets_manager_arn"]; !strings.Contains(got.(string), ":000011112222:") {
		t.Fatalf("the repository got %v", got)
	}
	if _, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: doc(t), AccountMap: map[string]string{"x": "y"}}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("bad map: %v", err)
	}
}

func TestInvalidBundleWritesNothing(t *testing.T) {
	r := newRig()
	good := string(doc(t, team("p", 0, "")))
	for name, b := range map[string]string{"tampered": strings.Replace(good, `"p"`, `"q"`, 1), "garbage": "nope"} {
		if _, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: json.RawMessage(b)}, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.audit.Entries)+len(r.outbox.Events)+len(r.repo.Records)+len(r.repo.Applied) != 0 {
		t.Fatal("a refused bundle wrote something")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	r := newRig()
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "imp-1", Operation: "CreateImport", RequestSHA256: [32]byte{7}},
			Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"dryRun":false}`), nil }}
	}
	in := imports.Input{Bundle: doc(t, team("p", 0, ""))}
	if _, err := r.svc.Run(t.Context(), admin, in, idem()); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Run(t.Context(), admin, in, idem())
	if err != nil || res.Replay == nil || len(r.repo.Applied) != 1 {
		t.Fatalf("replay %+v %v applied %d", res, err, len(r.repo.Applied))
	}
}

func TestCatalogIsAppliedAndRolledBackInDryRun(t *testing.T) {
	for _, dry := range []bool{false, true} {
		r := newRig()
		_, d, _ := domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: "x", ExternalSystems: []domain.Row{{"slug": "github"}}, Teams: []domain.TeamBundle{}}.Seal()
		if _, err := r.svc.Run(t.Context(), admin, imports.Input{Bundle: d, DryRun: dry}, nil); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range r.audit.Entries {
			if e.Record.Action == "ImportResource" {
				n++
			}
		}
		if len(r.repo.Catalog) != 1 || (dry && n != 0) || (!dry && n != 1) {
			t.Fatalf("dry=%v catalog %d audits %d", dry, len(r.repo.Catalog), n)
		}
	}
}
