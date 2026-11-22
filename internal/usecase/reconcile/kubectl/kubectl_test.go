package kubectl_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

const (
	roleA = "arn:aws:iam::111122223333:role/afs-viewers-okta"
	roleB = "arn:aws:iam::111122223333:role/afs-viewers-entra"
)

// world is an in-memory team table, cluster and EKS entry set, and the order of the calls made on them.
type world struct {
	teams                                        map[string]kubectl.Team
	bindings                                     map[string][]string // namespace -> groups of the RoleBinding
	entries                                      map[string]string   // role ARN -> group
	calls                                        []string
	failApply, failRemove, failEntries, failList error
}

func newWorld(ts ...kubectl.Team) *world {
	w := &world{teams: map[string]kubectl.Team{}, bindings: map[string][]string{}, entries: map[string]string{}}
	for _, t := range ts {
		w.teams[t.ID] = t
	}
	return w
}

func (w *world) LoadTeam(_ context.Context, id string) (kubectl.Team, error) {
	t, ok := w.teams[id]
	if !ok {
		return kubectl.Team{}, domain.NotFound("team")
	}
	return t, nil
}

func (w *world) ListViewerTeams(context.Context) ([]kubectl.Team, error) {
	if w.failList != nil {
		return nil, w.failList
	}
	var out []kubectl.Team
	for _, t := range w.teams {
		if t.ViewerKubectl {
			out = append(out, t)
		}
	}
	return out, nil
}

func (w *world) ApplyViewerAccess(_ context.Context, ns string, _ kubectl.Team, groups []string) (kubectl.ApplyResult, error) {
	w.calls = append(w.calls, "apply:"+ns)
	if w.failApply != nil {
		return kubectl.ApplyResult{}, w.failApply
	}
	w.bindings[ns] = groups
	return kubectl.ApplyResult{Changed: []string{"RoleBinding/" + ns + "/afs-viewer"}}, nil
}

func (w *world) RemoveViewerAccess(_ context.Context, ns string) (kubectl.ApplyResult, error) {
	w.calls = append(w.calls, "remove:"+ns)
	if w.failRemove != nil {
		return kubectl.ApplyResult{}, w.failRemove
	}
	delete(w.bindings, ns)
	return kubectl.ApplyResult{}, nil
}

func (w *world) SyncViewerEntries(_ context.Context, want map[string]string) ([]string, error) {
	w.calls = append(w.calls, "sync")
	if w.failEntries != nil {
		return nil, w.failEntries
	}
	w.entries = map[string]string{}
	for k, v := range want {
		w.entries[k] = v
	}
	return nil, nil
}

func (w *world) controller(t *testing.T, roles map[string]string) *kubectl.Controller {
	t.Helper()
	c, err := kubectl.NewController(kubectl.Config{Source: w, Cluster: w, Entries: w, ProviderRoles: roles})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func team(id string, on bool, providers ...kubectl.Provider) kubectl.Team {
	slug := strings.TrimPrefix(id, "team_")
	return kubectl.Team{ID: id, Slug: slug, Namespace: "afs-t-" + slug, State: "active", ViewerKubectl: on, Providers: providers}
}

var defaultIdP = kubectl.Provider{ID: "idp_01JD0000000000000000000001", IsDefault: true}

func reconcile(t *testing.T, c *kubectl.Controller, id string) error {
	t.Helper()
	out, err := c.Reconcile(t.Context(), engine.NewRef(kubectl.Kind, id, id))
	if !out.NoStatus && err == nil {
		t.Error("the controller must not write a status row")
	}
	return err
}

func sorted(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RC-US-11.1: with the flag on and a configured role the entry and the RoleBinding exist; off removes both.
func TestViewerAccessFollowsTheFlag(t *testing.T) {
	w := newWorld(team("team_a", true, defaultIdP))
	c := w.controller(t, map[string]string{kubectl.DefaultProvider: roleA})
	if err := reconcile(t, c, "team_a"); err != nil {
		t.Fatal(err)
	}
	if got := w.bindings["afs-t-a"]; !slices.Equal(got, []string{kubectl.GroupFor(roleA)}) {
		t.Fatalf("binding groups = %v", got)
	}
	if got := w.entries[roleA]; got != kubectl.GroupFor(roleA) || len(w.entries) != 1 {
		t.Fatalf("entries = %v", w.entries)
	}

	tm := w.teams["team_a"]
	tm.ViewerKubectl = false
	w.teams["team_a"] = tm
	w.calls = nil
	if err := reconcile(t, c, "team_a"); err != nil {
		t.Fatal(err)
	}
	if len(w.bindings) != 0 || len(w.entries) != 0 {
		t.Fatalf("access remains: bindings %v entries %v", w.bindings, w.entries)
	}
	// Turning access off ends it at the namespace first, then the entry.
	if want := []string{"remove:afs-t-a", "sync"}; !slices.Equal(w.calls, want) {
		t.Errorf("calls = %v, want %v", w.calls, want)
	}
}

// OQ-9: no configured role, no access entry and no RoleBinding, whatever the flag says.
func TestNoAccessWithoutAConfiguredRole(t *testing.T) {
	for name, roles := range map[string]map[string]string{
		"no roles at all":         nil,
		"role for another idp":    {"idp_01JD0000000000000000000009": roleA},
		"default but not default": {kubectl.DefaultProvider: roleA},
	} {
		t.Run(name, func(t *testing.T) {
			p := kubectl.Provider{ID: "idp_01JD0000000000000000000002"} // enabled, not the default
			w := newWorld(team("team_a", true, p))
			if name == "no roles at all" {
				w = newWorld(team("team_a", true, defaultIdP))
			}
			if err := reconcile(t, w.controller(t, roles), "team_a"); err != nil {
				t.Fatal(err)
			}
			if len(w.entries) != 0 || len(w.bindings) != 0 {
				t.Fatalf("access without a role: bindings %v entries %v", w.bindings, w.entries)
			}
			if !slices.Contains(w.calls, "remove:afs-t-a") {
				t.Errorf("stale access was not removed: %v", w.calls)
			}
		})
	}
}

func TestRolesPerProvider(t *testing.T) {
	own := kubectl.Provider{ID: "idp_01JD0000000000000000000003"}
	w := newWorld(team("team_a", true, defaultIdP, own))
	roles := map[string]string{kubectl.DefaultProvider: roleA, own.ID: roleB}
	if err := reconcile(t, w.controller(t, roles), "team_a"); err != nil {
		t.Fatal(err)
	}
	if got := w.bindings["afs-t-a"]; len(got) != 2 || got[0] == got[1] {
		t.Fatalf("groups = %v", got)
	}
	if got := sorted(w.entries); !slices.Equal(got, []string{roleB, roleA}) && !slices.Equal(got, []string{roleA, roleB}) {
		t.Fatalf("entries = %v", got)
	}
	// An id match wins over the default key for the default provider.
	got := kubectl.RolesFor(kubectl.Team{Providers: []kubectl.Provider{{ID: "idp_x", IsDefault: true}}}, map[string]string{"idp_x": roleB, kubectl.DefaultProvider: roleA})
	if !slices.Equal(got, []string{roleB}) {
		t.Errorf("RolesFor = %v, want the provider's own role", got)
	}
	if kubectl.GroupFor(roleA) == kubectl.GroupFor(roleB) || kubectl.GroupFor(roleA) != kubectl.GroupFor(roleA+"") {
		t.Error("GroupFor must be deterministic and distinct per role")
	}
	if g := kubectl.GroupFor(roleA); !strings.HasPrefix(g, "afs-kubectl-viewers-") || strings.HasPrefix(g, "system:") {
		t.Errorf("group %q", g)
	}
}

// One entry serves every team that uses the role: it stays while one team needs it and goes with the last.
func TestSharedRoleEntryOutlivesOneTeam(t *testing.T) {
	w := newWorld(team("team_a", true, defaultIdP), team("team_b", true, defaultIdP))
	c := w.controller(t, map[string]string{kubectl.DefaultProvider: roleA})
	for _, id := range []string{"team_a", "team_b"} {
		if err := reconcile(t, c, id); err != nil {
			t.Fatal(err)
		}
	}
	off := func(id string) {
		tm := w.teams[id]
		tm.ViewerKubectl = false
		w.teams[id] = tm
		if err := reconcile(t, c, id); err != nil {
			t.Fatal(err)
		}
	}
	off("team_a")
	if _, ok := w.entries[roleA]; !ok {
		t.Fatal("the entry went while team_b still needs it")
	}
	if _, ok := w.bindings["afs-t-a"]; ok {
		t.Fatal("team_a keeps its RoleBinding")
	}
	if _, ok := w.bindings["afs-t-b"]; !ok {
		t.Fatal("team_b lost its RoleBinding")
	}
	off("team_b")
	if len(w.entries) != 0 {
		t.Fatalf("the entry stayed after the last team: %v", w.entries)
	}
}

func TestTeamStates(t *testing.T) {
	cases := []struct {
		state       string
		wantBinding bool
	}{{"active", true}, {"suspended", true}, {"deleting", false}, {"deleted", false}}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			tm := team("team_a", true, defaultIdP)
			tm.State = tc.state
			w := newWorld(tm)
			if err := reconcile(t, w.controller(t, map[string]string{kubectl.DefaultProvider: roleA}), "team_a"); err != nil {
				t.Fatal(err)
			}
			if _, got := w.bindings["afs-t-a"]; got != tc.wantBinding {
				t.Errorf("binding = %v, want %v", got, tc.wantBinding)
			}
			if _, got := w.entries[roleA]; got != tc.wantBinding {
				t.Errorf("entry = %v, want %v", got, tc.wantBinding)
			}
			if !tc.wantBinding && (slices.Contains(w.calls, "apply:afs-t-a") || slices.Contains(w.calls, "remove:afs-t-a")) {
				t.Errorf("a team that is going away had its namespace touched: %v", w.calls)
			}
		})
	}
}

func TestUnknownTeamStillSyncsEntries(t *testing.T) {
	w := newWorld()
	w.entries[roleA] = kubectl.GroupFor(roleA) // left behind by a team that is gone
	if err := reconcile(t, w.controller(t, map[string]string{kubectl.DefaultProvider: roleA}), "team_gone"); err != nil {
		t.Fatal(err)
	}
	if len(w.entries) != 0 {
		t.Errorf("an orphaned entry stayed: %v", w.entries)
	}
}

func TestFailuresAreReportedAndNothingIsHidden(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(*world){
		"apply":   func(w *world) { w.failApply = boom },
		"entries": func(w *world) { w.failEntries = boom },
		"list":    func(w *world) { w.failList = boom },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(team("team_a", true, defaultIdP))
			set(w)
			err := reconcile(t, w.controller(t, map[string]string{kubectl.DefaultProvider: roleA}), "team_a")
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("remove", func(t *testing.T) {
		w := newWorld(team("team_a", false, defaultIdP))
		w.failRemove = boom
		if err := reconcile(t, w.controller(t, map[string]string{kubectl.DefaultProvider: roleA}), "team_a"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if !slices.Contains(w.calls, "sync") {
			t.Error("a failed removal must not stop the entry sync")
		}
	})
	t.Run("load", func(t *testing.T) {
		c, _ := kubectl.NewController(kubectl.Config{Source: failingSource{boom}, Cluster: &world{}, Entries: &world{}})
		if _, err := c.Reconcile(t.Context(), engine.NewRef(kubectl.Kind, "x", "x")); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

type failingSource struct{ err error }

func (f failingSource) LoadTeam(context.Context, string) (kubectl.Team, error) {
	return kubectl.Team{}, f.err
}
func (failingSource) ListViewerTeams(context.Context) ([]kubectl.Team, error) { return nil, nil }

func TestNewControllerNeedsItsPorts(t *testing.T) {
	if _, err := kubectl.NewController(kubectl.Config{}); err == nil {
		t.Error("an empty config was accepted")
	}
}

func TestRoutesBuildARegistry(t *testing.T) {
	if _, err := engine.NewRegistry(kubectl.Routes()...); err != nil {
		t.Fatal(err)
	}
}
