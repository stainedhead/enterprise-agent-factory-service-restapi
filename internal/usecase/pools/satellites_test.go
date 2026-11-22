package pools_test

import (
	"encoding/json"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func keyOf(key, op string) port.IdempotencyKey {
	return port.IdempotencyKey{Key: key, Operation: op, TeamID: team}
}

func render(v any) (int, json.RawMessage, error) { b, e := json.Marshal(v); return 201, b, e }

func lit(v string) pools.ConfigInput {
	return pools.ConfigInput{Source: domain.ConfigSourceLiteral, Value: &v}
}

func ver(v domain.Version) *domain.Version { return &v }

func TestStages(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.svc.CreateStage(ctx, caller, persist, domain.NewStageParams{Name: "triage", Position: 1}, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("persistent team: %v", err)
	}
	for _, bad := range []domain.NewStageParams{{Name: "Bad Name", Position: 1}, {Name: "ok", Position: 0}, {Name: "ok", Position: 51}} {
		if _, err := r.svc.CreateStage(ctx, caller, team, bad, nil); domain.KindOf(err) != domain.KindValidation {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if a, e := r.writes(); a != 0 || e != 0 {
		t.Fatal("refused stage creates wrote audit or events")
	}
	one, err := r.svc.CreateStage(ctx, caller, team, domain.NewStageParams{Name: "triage", Position: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.CreateStage(ctx, caller, team, domain.NewStageParams{Name: "fix", Position: 1}, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate position: %v", err)
	}
	if a, e := r.writes(); a != 1 || e != 0 {
		t.Errorf("a stage create writes one audit record and no event (CD-7): %d, %d", a, e)
	}
	two, err := r.svc.CreateStage(ctx, caller, team, domain.NewStageParams{Name: "fix", Position: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, next, err := r.svc.ListStages(ctx, caller, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("page: %v %v %q", rows, err, next)
	}
	if rows, _, _ = r.svc.ListStages(ctx, caller, team, acting.Page{Cursor: next, Limit: 10}); len(rows) != 1 || rows[0].ID != two.Stage.ID {
		t.Errorf("second page: %v", rows)
	}

	desc, pos := "first", 3
	up := []struct {
		name   string
		team   string
		id     string
		expect domain.Version
		patch  domain.StagePatch
		want   domain.ErrorKind
	}{
		{"ok", team, one.Stage.ID, one.Stage.Version, domain.StagePatch{Description: &desc, Position: &pos}, ""},
		{"stale", team, two.Stage.ID, 9, domain.StagePatch{Description: &desc}, domain.KindPreconditionFailed},
		{"persistent", persist, two.Stage.ID, two.Stage.Version, domain.StagePatch{Description: &desc}, domain.KindConflict},
		{"unknown", team, "stg_nope", 1, domain.StagePatch{Description: &desc}, domain.KindNotFound},
		{"position out of range", team, two.Stage.ID, two.Stage.Version, domain.StagePatch{Position: new(int)}, domain.KindValidation},
	}
	for _, tc := range up {
		t.Run("update "+tc.name, func(t *testing.T) {
			got, err := r.svc.UpdateStage(ctx, caller, tc.team, tc.id, tc.expect, tc.patch)
			if tc.want != "" {
				kind(t, err, tc.want)
				return
			}
			if err != nil || got.Description != "first" || got.Position != 3 || got.Version != tc.expect+1 {
				t.Errorf("%+v %v", got, err)
			}
		})
	}

	r.repo.StageUse[two.Stage.ID] = true
	del := []struct {
		name   string
		team   string
		id     string
		expect domain.Version
		want   domain.ErrorKind
	}{
		{"in use", team, two.Stage.ID, two.Stage.Version, domain.KindConflict},
		{"stale", team, one.Stage.ID, 99, domain.KindPreconditionFailed},
		{"persistent", persist, one.Stage.ID, 2, domain.KindConflict},
		{"unknown", team, "stg_nope", 1, domain.KindNotFound},
		{"ok", team, one.Stage.ID, 2, ""},
	}
	for _, tc := range del {
		t.Run("delete "+tc.name, func(t *testing.T) {
			err := r.svc.DeleteStage(ctx, caller, tc.team, tc.id, tc.expect)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" {
				kind(t, err, tc.want)
			}
		})
	}
	if _, ok := r.repo.Stages[one.Stage.ID]; ok {
		t.Error("the stage was not deleted")
	}
}

func TestStageCreateIdempotent(t *testing.T) {
	r := newRig(t)
	idem := &acting.Idem{Key: keyOf("k1", "CreateStage"), Render: render}
	a, err := r.svc.CreateStage(t.Context(), caller, team, domain.NewStageParams{Name: "triage", Position: 1}, idem)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.CreateStage(t.Context(), caller, team, domain.NewStageParams{Name: "triage", Position: 1}, &acting.Idem{Key: keyOf("k1", "CreateStage"), Render: render})
	if err != nil || b.Replay == nil || len(r.repo.Stages) != 1 || a.Stage.ID == "" {
		t.Fatalf("replay: %+v %v", b, err)
	}
}

func TestHandoffs(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a, b, c := r.pool(t, "intake"), r.pool(t, "fixer"), r.pool(t, "review")
	if h, err := r.svc.ListHandoffs(ctx, caller, team, a.ID); err != nil || len(h.ToPoolIDs) != 0 {
		t.Fatalf("empty: %+v %v", h, err)
	}
	tests := []struct {
		name   string
		team   string
		pool   string
		expect *domain.Version
		to     []string
		want   domain.ErrorKind
	}{
		{"persistent team", persist, a.ID, nil, []string{b.ID}, domain.KindConflict},
		{"unknown pool", team, "pool_nope", nil, nil, domain.KindNotFound},
		{"to itself", team, a.ID, nil, []string{a.ID}, domain.KindValidation},
		{"to a pool that is not there", team, a.ID, nil, []string{"pool_01JD2Q3R4S5T6V7W8X9Y0Z0099"}, domain.KindValidation},
		{"stale version", team, a.ID, ver(77), []string{b.ID}, domain.KindPreconditionFailed},
		{"ok", team, a.ID, nil, []string{c.ID, b.ID}, ""},
		{"replace and clear", team, a.ID, nil, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a0, e0 := r.writes()
			h, err := r.svc.PutHandoffs(ctx, caller, tc.team, tc.pool, tc.expect, tc.to)
			if tc.want != "" {
				kind(t, err, tc.want)
				if a1, e1 := r.writes(); a1 != a0 || e1 != e0 {
					t.Error("a refused change wrote audit or events")
				}
				return
			}
			if err != nil || len(h.ToPoolIDs) != len(tc.to) {
				t.Fatalf("%+v %v", h, err)
			}
			if a1, e1 := r.writes(); a1 != a0+1 || e1 != e0+1 || r.outbox.Events[e0].Type != domain.EventPoolUpdated {
				t.Errorf("one audit record and pool.updated expected, got %d, %d", a1-a0, e1-e0)
			}
		})
	}
	if _, err := r.svc.PutHandoffs(ctx, caller, team, a.ID, nil, []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	if h, _ := r.svc.ListHandoffs(ctx, caller, team, a.ID); len(h.ToPoolIDs) != 1 || h.ToPoolIDs[0] != b.ID {
		t.Errorf("list: %+v", h)
	}
	// A pool being deleted cannot change.
	if _, err := r.svc.Delete(ctx, caller, team, b.ID, r.repo.Pools[b.ID].Version); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutHandoffs(ctx, caller, team, b.ID, nil, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("deleting pool: %v", err)
	}
}

func TestBindings(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	p, other := r.pool(t, "intake"), r.pool(t, "fixer")
	const (
		shared   = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"
		own      = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0002"
		agentSec = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0003"
		foreign  = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0005"
	)
	r.repo.Secrets_[shared] = pools.SecretInfo{}
	r.repo.Secrets_[own] = pools.SecretInfo{PoolID: p.ID}
	r.repo.Secrets_[agentSec] = pools.SecretInfo{AgentID: "agt_1"}
	r.repo.Secrets_[foreign] = pools.SecretInfo{PoolID: other.ID}
	env := func(id, target string) domain.SecretBinding {
		return domain.SecretBinding{SecretID: id, Delivery: "env", Target: target}
	}
	tests := []struct {
		name   string
		team   string
		expect *domain.Version
		bs     []domain.SecretBinding
		want   domain.ErrorKind
	}{
		{"persistent team", persist, nil, []domain.SecretBinding{env(shared, "TOKEN")}, domain.KindConflict},
		{"agent secret", team, nil, []domain.SecretBinding{env(agentSec, "TOKEN")}, domain.KindValidation},
		{"another pool's secret", team, nil, []domain.SecretBinding{env(foreign, "TOKEN")}, domain.KindValidation},
		{"unknown secret", team, nil, []domain.SecretBinding{env("sec_01JD2Q3R4S5T6V7W8X9Y0Z0009", "TOKEN")}, domain.KindValidation},
		{"bad target", team, nil, []domain.SecretBinding{env(shared, "lower")}, domain.KindValidation},
		{"duplicate target", team, nil, []domain.SecretBinding{env(shared, "TOKEN"), env(own, "TOKEN")}, domain.KindValidation},
		{"stale", team, ver(50), []domain.SecretBinding{env(shared, "TOKEN")}, domain.KindPreconditionFailed},
		{"shared and own", team, nil, []domain.SecretBinding{env(shared, "TOKEN"), {SecretID: own, Delivery: "file", Target: "/etc/pool/key"}}, ""},
		{"clear", team, nil, nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a0, e0 := r.writes()
			got, err := r.svc.PutBindings(ctx, caller, tc.team, p.ID, tc.expect, tc.bs)
			if tc.want != "" {
				kind(t, err, tc.want)
				if a1, e1 := r.writes(); a1 != a0 || e1 != e0 {
					t.Error("a refused change wrote audit or events")
				}
				return
			}
			if err != nil || len(got.Items) != len(tc.bs) {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	if _, err := r.svc.PutBindings(ctx, caller, team, p.ID, nil, []domain.SecretBinding{env(shared, "TOKEN")}); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.ListBindings(ctx, caller, team, p.ID)
	if err != nil || len(got.Items) != 1 || got.Version == 0 {
		t.Fatalf("list: %+v %v", got, err)
	}
	if _, err := r.svc.ListBindings(ctx, caller, team, "pool_nope"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown pool: %v", err)
	}
}

func TestConfig(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	p := r.pool(t, "intake")
	v := "high"
	litv := lit(v)
	r.repo.Namespace[team+"/REGION"] = true
	tests := []struct {
		name   string
		team   string
		cfg    string
		in     pools.ConfigInput
		expect *domain.Version
		want   domain.ErrorKind
	}{
		{"persistent team", persist, "LEVEL", litv, nil, domain.KindConflict},
		{"bad name", team, "lower", litv, nil, domain.KindValidation},
		{"literal without value", team, "LEVEL", pools.ConfigInput{Source: domain.ConfigSourceLiteral}, nil, domain.KindValidation},
		{"link with value", team, "REGION", pools.ConfigInput{Source: domain.ConfigSourceNamespace, Value: &v}, nil, domain.KindValidation},
		{"unknown source", team, "LEVEL", pools.ConfigInput{Source: "other"}, nil, domain.KindValidation},
		{"link to a missing namespace value", team, "GONE", pools.ConfigInput{Source: domain.ConfigSourceNamespace}, nil, domain.KindValidation},
		{"If-Match on a new entry", team, "LEVEL", litv, ver(1), domain.KindPreconditionFailed},
		{"create", team, "LEVEL", litv, nil, ""},
		{"link", team, "REGION", pools.ConfigInput{Source: domain.ConfigSourceNamespace}, nil, ""},
		{"stale update", team, "LEVEL", litv, ver(9), domain.KindPreconditionFailed},
		{"update", team, "LEVEL", lit("low"), ver(1), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a0, e0 := r.writes()
			got, err := r.svc.PutConfig(ctx, caller, tc.team, p.ID, domain.ConfigName(tc.cfg), tc.in, tc.expect)
			if tc.want != "" {
				kind(t, err, tc.want)
				if a1, e1 := r.writes(); a1 != a0 || e1 != e0 {
					t.Error("a refused change wrote audit or events")
				}
				return
			}
			if err != nil || got.Name != domain.ConfigName(tc.cfg) {
				t.Fatalf("%+v %v", got, err)
			}
			if a1, e1 := r.writes(); a1 != a0+1 || e1 != e0 {
				t.Errorf("one audit record and no event (CD-7) expected: %d, %d", a1-a0, e1-e0)
			}
		})
	}
	rows, next, err := r.svc.ListConfig(ctx, caller, team, p.ID, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("page: %v %v %q", rows, err, next)
	}
	if _, _, err := r.svc.ListConfig(ctx, caller, team, "pool_nope", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown pool: %v", err)
	}
	cur := r.repo.Configs[p.ID+"/LEVEL"]
	del := []struct {
		name   string
		team   string
		cfg    string
		expect domain.Version
		want   domain.ErrorKind
	}{
		{"persistent team", persist, "LEVEL", cur.Version, domain.KindConflict},
		{"unknown", team, "GONE", 1, domain.KindNotFound},
		{"stale", team, "LEVEL", cur.Version + 5, domain.KindPreconditionFailed},
		{"ok", team, "LEVEL", cur.Version, ""},
	}
	for _, tc := range del {
		t.Run("delete "+tc.name, func(t *testing.T) {
			err := r.svc.DeleteConfig(ctx, caller, tc.team, p.ID, domain.ConfigName(tc.cfg), tc.expect)
			if tc.want != "" {
				kind(t, err, tc.want)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExternalSystems(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	p := r.pool(t, "intake")
	on := domain.ExternalSystemAccess{ExternalSystem: domain.ExternalSystem{ID: "ext_1", Slug: "github", Version: 1}}
	teamOff := domain.ExternalSystemAccess{ExternalSystem: domain.ExternalSystem{ID: "ext_2", Slug: "jira", Version: 1}, DisabledAt: domain.ExtSystemDisabledAtTeam}
	r.repo.Systems["ext_1"], r.repo.Systems["ext_2"] = on, teamOff
	rows, next, err := r.svc.ListSystems(ctx, caller, team, p.ID, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("list: %v %v %q", rows, err, next)
	}
	tests := []struct {
		name    string
		team    string
		system  string
		enabled bool
		expect  *domain.Version
		want    domain.ErrorKind
		off     bool
	}{
		{"persistent team", persist, "ext_1", false, nil, domain.KindConflict, false},
		{"unknown system", team, "ext_9", false, nil, domain.KindNotFound, false},
		{"stale", team, "ext_1", false, ver(5), domain.KindPreconditionFailed, false},
		{"switch on what the team has off", team, "ext_2", true, nil, domain.KindConflict, false},
		{"switch off", team, "ext_1", false, nil, "", true},
		{"switch off again", team, "ext_1", false, ver(1), "", true},
		{"switch back on", team, "ext_1", true, nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a0, _ := r.writes()
			got, err := r.svc.SetSystemState(ctx, caller, tc.team, p.ID, tc.system, tc.enabled, tc.expect)
			if tc.want != "" {
				kind(t, err, tc.want)
				if a1, _ := r.writes(); a1 != a0 {
					t.Error("a refused change wrote an audit record")
				}
				return
			}
			if err != nil || got.On() == tc.off {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	if _, _, err := r.svc.ListSystems(ctx, caller, team, "pool_nope", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown pool: %v", err)
	}
}

func TestRoleLinks(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	p := r.pool(t, "intake")
	r.repo.Allowed["444455556666"] = true
	in := func(role, acct, profile string) domain.NewPoolRoleLinkParams {
		return domain.NewPoolRoleLinkParams{RoleName: role, AccountID: acct, ProfileName: profile}
	}
	tests := []struct {
		name string
		team string
		in   domain.NewPoolRoleLinkParams
		want domain.ErrorKind
	}{
		{"persistent team", persist, in("shared", account, "one"), domain.KindConflict},
		{"account outside the allowed accounts", team, in("shared", "999988887777", "one"), domain.KindValidation},
		{"bad role name", team, in("bad role", account, "one"), domain.KindValidation},
		{"bad profile", team, in("shared", account, "Bad_Profile"), domain.KindValidation},
		{"first link", team, in("shared", account, "one"), ""},
		{"a role name other than the pool's", team, in("other", "444455556666", "two"), domain.KindValidation},
		{"same account twice", team, in("shared", account, "again"), domain.KindConflict},
		{"second account", team, in("shared", "444455556666", "two"), ""},
	}
	var first domain.PoolRoleLink
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a0, e0 := r.writes()
			got, err := r.svc.CreateLink(ctx, caller, tc.team, p.ID, tc.in, nil)
			if tc.want != "" {
				kind(t, err, tc.want)
				if a1, e1 := r.writes(); a1 != a0 || e1 != e0 {
					t.Error("a refused link wrote audit or events")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Link.RoleARN != "arn:aws:iam::"+tc.in.AccountID+":role/"+tc.in.RoleName || got.Link.Status != domain.RoleLinkStatusPendingTrust || got.Link.ExternalID == "" {
				t.Errorf("link: %+v", got.Link)
			}
			if a1, e1 := r.writes(); a1 != a0+1 || e1 != e0 {
				t.Errorf("one audit record and no event expected: %d, %d", a1-a0, e1-e0)
			}
			if first.ID == "" {
				first = got.Link
			}
		})
	}
	rows, next, err := r.svc.ListLinks(ctx, caller, team, p.ID, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("list: %v %v %q", rows, err, next)
	}
	if _, _, err := r.svc.ListLinks(ctx, caller, team, "pool_nope", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown pool: %v", err)
	}

	// Verify: back to pending_trust; a disabled link is a conflict; persistent teams are refused.
	l := r.repo.Links[first.ID]
	l.Status, l.LastError = domain.RoleLinkStatusFailed, "trust missing"
	r.repo.Links[first.ID] = l
	got, err := r.svc.VerifyLink(ctx, caller, team, p.ID, first.ID, nil)
	if err != nil || got.Link.Status != domain.RoleLinkStatusPendingTrust || got.Link.LastError != "" {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if _, err := r.svc.VerifyLink(ctx, caller, persist, p.ID, first.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("persistent: %v", err)
	}
	if _, err := r.svc.VerifyLink(ctx, caller, team, p.ID, "prl_nope", nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown link: %v", err)
	}
	if _, err := r.svc.VerifyLink(ctx, caller, team, "pool_nope", first.ID, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown pool: %v", err)
	}
	l.Status = domain.RoleLinkStatusDisabled
	r.repo.Links[first.ID] = l
	if _, err := r.svc.VerifyLink(ctx, caller, team, p.ID, first.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("disabled: %v", err)
	}

	cur := r.repo.Links[first.ID]
	for _, tc := range []struct {
		name   string
		team   string
		id     string
		expect domain.Version
		want   domain.ErrorKind
	}{
		{"persistent", persist, first.ID, cur.Version, domain.KindConflict},
		{"unknown", team, "prl_nope", 1, domain.KindNotFound},
		{"stale", team, first.ID, cur.Version + 4, domain.KindPreconditionFailed},
		{"ok", team, first.ID, cur.Version, ""},
	} {
		t.Run("delete "+tc.name, func(t *testing.T) {
			err := r.svc.DeleteLink(ctx, caller, tc.team, p.ID, tc.id, tc.expect)
			if tc.want != "" {
				kind(t, err, tc.want)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
