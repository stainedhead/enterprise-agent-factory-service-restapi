package pools_test

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
)

const sharedID = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"

// fixture is a team with every kind of satellite already in place, so each operation reaches every
// repository method it uses.
type fixture struct {
	*rig
	p, q  domain.Pool
	stage domain.Stage
	link  domain.PoolRoleLink
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	r := newRig(t)
	ctx := t.Context()
	f := fixture{rig: r, p: r.pool(t, "fixer"), q: r.pool(t, "other")}
	st, err := r.svc.CreateStage(ctx, caller, team, domain.NewStageParams{Name: "triage", Position: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.stage = st.Stage
	if _, err := r.svc.PutConfig(ctx, caller, team, f.p.ID, "LEVEL", lit("x"), nil); err != nil {
		t.Fatal(err)
	}
	l, err := r.svc.CreateLink(ctx, caller, team, f.p.ID, domain.NewPoolRoleLinkParams{RoleName: "shared", AccountID: account, ProfileName: "one"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.link = l.Link
	r.repo.Systems["ext_1"] = domain.ExternalSystemAccess{ExternalSystem: domain.ExternalSystem{ID: "ext_1", Slug: "github", Version: 1}}
	r.repo.Secrets_[sharedID] = pools.SecretInfo{}
	r.repo.Namespace[team+"/REGION"] = true
	r.repo.Allowed["444455556666"] = true
	f.p = r.repo.Pools[f.p.ID]
	return f
}

// TestRepositoryFailuresAreReturnedAndWriteNothing fails each repository method in turn under every
// operation: the error reaches the caller and a mutation leaves no audit record or event behind.
func TestRepositoryFailuresAreReturnedAndWriteNothing(t *testing.T) {
	ctx := t.Context()
	page := acting.Page{Limit: 5}
	type op struct {
		name  string
		write bool
		run   func(f fixture) error
	}
	bind := domain.SecretBinding{SecretID: sharedID, Delivery: "env", Target: "TOKEN"}
	w := 5
	desc := "d"
	ops := []op{
		{"List", false, func(f fixture) error { _, _, err := f.svc.List(ctx, caller, team, page); return err }},
		{"Get", false, func(f fixture) error { _, err := f.svc.Get(ctx, caller, team, f.p.ID); return err }},
		{"GetStatus", false, func(f fixture) error { _, err := f.svc.GetStatus(ctx, caller, team, f.p.ID); return err }},
		{"Create", true, func(f fixture) error {
			in := params("third")
			in.StageID = f.stage.ID
			_, err := f.svc.Create(ctx, caller, team, in, nil)
			return err
		}},
		{"Update", true, func(f fixture) error {
			tpl := "claude-code"
			_, err := f.svc.Update(ctx, caller, team, f.p.ID, f.p.Version, domain.PoolPatch{Workers: &w, Template: &tpl, Description: &desc,
				StageID: domain.OptionalString{Set: true, Value: &f.stage.ID}})
			return err
		}},
		{"Delete", true, func(f fixture) error { _, err := f.svc.Delete(ctx, caller, team, f.p.ID, f.p.Version); return err }},
		{"Scale", true, func(f fixture) error { _, err := f.svc.Scale(ctx, caller, team, f.p.ID, 9, nil); return err }},
		{"Pause", true, func(f fixture) error { _, err := f.svc.Pause(ctx, caller, team, f.p.ID, nil); return err }},
		{"ListStages", false, func(f fixture) error { _, _, err := f.svc.ListStages(ctx, caller, team, page); return err }},
		{"CreateStage", true, func(f fixture) error {
			_, err := f.svc.CreateStage(ctx, caller, team, domain.NewStageParams{Name: "fix", Position: 2}, nil)
			return err
		}},
		{"UpdateStage", true, func(f fixture) error {
			_, err := f.svc.UpdateStage(ctx, caller, team, f.stage.ID, f.stage.Version, domain.StagePatch{Description: &desc})
			return err
		}},
		{"DeleteStage", true, func(f fixture) error { return f.svc.DeleteStage(ctx, caller, team, f.stage.ID, f.stage.Version) }},
		{"ListHandoffs", false, func(f fixture) error { _, err := f.svc.ListHandoffs(ctx, caller, team, f.p.ID); return err }},
		{"PutHandoffs", true, func(f fixture) error {
			_, err := f.svc.PutHandoffs(ctx, caller, team, f.p.ID, nil, []string{f.q.ID})
			return err
		}},
		{"ListBindings", false, func(f fixture) error { _, err := f.svc.ListBindings(ctx, caller, team, f.p.ID); return err }},
		{"PutBindings", true, func(f fixture) error {
			_, err := f.svc.PutBindings(ctx, caller, team, f.p.ID, nil, []domain.SecretBinding{bind})
			return err
		}},
		{"ListConfig", false, func(f fixture) error { _, _, err := f.svc.ListConfig(ctx, caller, team, f.p.ID, page); return err }},
		{"PutConfig new", true, func(f fixture) error {
			_, err := f.svc.PutConfig(ctx, caller, team, f.p.ID, "OTHER", lit("y"), nil)
			return err
		}},
		{"PutConfig link", true, func(f fixture) error {
			_, err := f.svc.PutConfig(ctx, caller, team, f.p.ID, "REGION", pools.ConfigInput{Source: domain.ConfigSourceNamespace}, nil)
			return err
		}},
		{"PutConfig update", true, func(f fixture) error {
			_, err := f.svc.PutConfig(ctx, caller, team, f.p.ID, "LEVEL", lit("z"), nil)
			return err
		}},
		{"DeleteConfig", true, func(f fixture) error {
			return f.svc.DeleteConfig(ctx, caller, team, f.p.ID, "LEVEL", f.repo.Configs[f.p.ID+"/LEVEL"].Version)
		}},
		{"ListSystems", false, func(f fixture) error { _, _, err := f.svc.ListSystems(ctx, caller, team, f.p.ID, page); return err }},
		{"SetSystemState", true, func(f fixture) error {
			_, err := f.svc.SetSystemState(ctx, caller, team, f.p.ID, "ext_1", false, nil)
			return err
		}},
		{"ListLinks", false, func(f fixture) error { _, _, err := f.svc.ListLinks(ctx, caller, team, f.p.ID, page); return err }},
		{"CreateLink", true, func(f fixture) error {
			_, err := f.svc.CreateLink(ctx, caller, team, f.p.ID, domain.NewPoolRoleLinkParams{RoleName: "shared", AccountID: "444455556666", ProfileName: "two"}, nil)
			return err
		}},
		{"VerifyLink", true, func(f fixture) error {
			_, err := f.svc.VerifyLink(ctx, caller, team, f.p.ID, f.link.ID, nil)
			return err
		}},
		{"DeleteLink", true, func(f fixture) error {
			return f.svc.DeleteLink(ctx, caller, team, f.p.ID, f.link.ID, f.repo.Links[f.link.ID].Version)
		}},
	}
	methods := []string{"AccountAllowed", "AgentNameTaken", "Bindings", "DeleteConfig", "DeleteLink", "DeleteStage", "Get", "GetConfig",
		"GetLink", "GetStage", "GetSystem", "Handoffs", "Insert", "InsertConfig", "InsertLink", "InsertStage", "LinkRoleName", "List",
		"ListConfig", "ListLinks", "ListStages", "ListSystems", "ModelEnabled", "NamespaceValueExists", "PoolsExist", "ReplaceBindings",
		"ReplaceHandoffs", "RequestVerify", "Secrets", "SetSystemOff", "TeamKind", "Template", "Touch", "Update", "UpdateConfig",
		"UpdateStage", "Workers"}
	boom := errors.New("boom")
	for _, o := range ops {
		for _, m := range methods {
			t.Run(o.name+"/"+m, func(t *testing.T) {
				f := newFixture(t)
				a0, e0 := f.writes()
				f.repo.Fail[m] = boom
				err := o.run(f)
				if err == nil {
					return // the operation does not call this method
				}
				if !errors.Is(err, boom) {
					t.Fatalf("error = %v, want the repository's", err)
				}
				if o.write {
					if a1, e1 := f.writes(); a1 != a0 || e1 != e0 {
						t.Errorf("a failed mutation left audit or events behind (%d/%d, %d/%d)", a1, a0, e1, e0)
					}
				}
			})
		}
	}
}
