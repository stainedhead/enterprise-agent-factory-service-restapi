package domain

import (
	"slices"
	"testing"
)

func sets() ModelSets {
	return ModelSets{Service: []string{"a", "b", "c"}, Team: []string{"a", "b"}, Callable: []string{"a", "b"}, SettingDefault: "b"}
}

func fieldReason(err error) (field, reason string) {
	fe := FieldErrors(err)
	if len(fe) == 0 {
		return "", ""
	}
	return fe[0].Field, fe[0].Reason
}

func TestModelAccessValid(t *testing.T) {
	if !ModelAccessAll.Valid() || !ModelAccessSelected.Valid() || ModelAccess("x").Valid() {
		t.Error("Valid")
	}
}

func TestModelsInputPresent(t *testing.T) {
	var nilIn *ModelsInput
	if nilIn.Present() || (&ModelsInput{}).Present() {
		t.Error("empty input is not present")
	}
	for _, in := range []*ModelsInput{{Access: ptr(ModelAccessAll)}, {ModelIDs: ptr([]string{})}, {DefaultModelID: ptr("a")}} {
		if !in.Present() {
			t.Errorf("%+v is present", in)
		}
	}
}

func TestResolveNewModels(t *testing.T) {
	sel := ModelAccessSelected
	all := ModelAccessAll
	tests := []struct {
		name       string
		in         *ModelsInput
		legacy     string
		sets       ModelSets
		wantAccess ModelAccess
		wantDef    string
		wantField  string
		wantReason string
	}{
		{"nothing: setting default", nil, "", sets(), ModelAccessAll, "b", "", ""},
		{"nothing: first callable when no setting", nil, "", ModelSets{Service: []string{"a", "b"}, Team: []string{"b", "a"}, Callable: []string{"a", "b"}}, ModelAccessAll, "a", "", ""},
		{"legacy alone", nil, "a", sets(), ModelAccessAll, "a", "", ""},
		{"list alone is a selection", &ModelsInput{ModelIDs: ptr([]string{"a"})}, "", sets(), ModelAccessSelected, "a", "", ""},
		{"selected picks the setting default when it is in the selection", &ModelsInput{Access: &sel, ModelIDs: ptr([]string{"a", "b"})}, "", sets(), ModelAccessSelected, "b", "", ""},
		{"selected picks the lowest id", &ModelsInput{Access: &sel, ModelIDs: ptr([]string{"b", "a"})}, "", ModelSets{Service: []string{"a", "b"}, Team: []string{"a", "b"}, Callable: []string{"a", "b"}}, ModelAccessSelected, "a", "", ""},
		{"explicit default", &ModelsInput{DefaultModelID: ptr("a")}, "a", sets(), ModelAccessAll, "a", "", ""},
		{"legacy conflicts", &ModelsInput{DefaultModelID: ptr("a")}, "b", sets(), "", "", "modelId", ReasonConflictingFields},
		{"bad access", &ModelsInput{Access: ptr(ModelAccess("x"))}, "", sets(), "", "", "models.access", "must be all or selected"},
		{"all with ids", &ModelsInput{Access: &all, ModelIDs: ptr([]string{"a"})}, "", sets(), "", "", "models.modelIds", ReasonModelIDsNotAllowedForAll},
		{"selected empty", &ModelsInput{Access: &sel}, "", sets(), "", "", "models.modelIds", ReasonModelsSelectionEmpty},
		{"not in service", &ModelsInput{ModelIDs: ptr([]string{"z"})}, "", sets(), "", "", "models.modelIds[0]", ReasonModelNotEnabledForService},
		{"not in team", &ModelsInput{ModelIDs: ptr([]string{"c"})}, "", sets(), "", "", "models.modelIds[0]", ReasonModelNotEnabledForTeam},
		{"no default available", nil, "", ModelSets{Service: []string{"a"}, Team: []string{"a"}}, "", "", "models.defaultModelId", "required"},
		{"default outside the team", &ModelsInput{DefaultModelID: ptr("c")}, "", sets(), "", "", "models.defaultModelId", ReasonDefaultModelNotAllowed},
		{"default outside the selection", &ModelsInput{Access: &sel, ModelIDs: ptr([]string{"a"}), DefaultModelID: ptr("b")}, "", sets(), "", "", "models.defaultModelId", ReasonDefaultModelNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveNewModels(tc.in, tc.legacy, tc.sets)
			if tc.wantField != "" {
				f, r := fieldReason(err)
				if f != tc.wantField || r != tc.wantReason {
					t.Fatalf("got %q %q (%v), want %q %q", f, r, err, tc.wantField, tc.wantReason)
				}
				return
			}
			if err != nil || got.Access != tc.wantAccess || got.DefaultModelID != tc.wantDef {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestSelectionChecks(t *testing.T) {
	fe := checkSelection([]string{"", "a", "a"}, sets(), "m")
	if len(fe) != 2 || fe[0].Reason != "must not be empty" || fe[1].Reason != "is listed twice" {
		t.Errorf("%+v", fe)
	}
	many := make([]string, MaxSelectedModels+1)
	for i := range many {
		many[i] = "a"
	}
	m := AgentModels{Access: ModelAccessSelected, ModelIDs: many}
	if fe := validateAccess(&m, sets()); len(fe) != 1 || fe[0].Field != "models.modelIds" {
		t.Errorf("too many: %+v", fe)
	}
	if fe := validateAccess(&AgentModels{Access: "x"}, sets()); len(fe) != 1 {
		t.Errorf("bad access: %+v", fe)
	}
	if firstOf(nil) != "" || firstOf([]string{"b", "a"}) != "a" {
		t.Error("firstOf")
	}
	if (ModelSets{Team: []string{"a"}, Callable: []string{"b"}}).firstCallable() != "" {
		t.Error("firstCallable of nothing")
	}
}

func TestApplyModels(t *testing.T) {
	cur := AgentModels{Access: ModelAccessSelected, ModelIDs: []string{"a"}, DefaultModelID: "a"}
	all := ModelAccessAll
	sel := ModelAccessSelected
	tests := []struct {
		name       string
		in         *ModelsInput
		legacy     string
		want       AgentModels
		wantReason string
	}{
		{"no change", nil, "", cur, ""},
		{"legacy sets only the default", nil, "a", cur, ""},
		{"to all clears the selection", &ModelsInput{Access: &all}, "", AgentModels{Access: ModelAccessAll, ModelIDs: []string{}, DefaultModelID: "a"}, ""},
		{"replace the list", &ModelsInput{ModelIDs: ptr([]string{"a", "b"})}, "", AgentModels{Access: ModelAccessSelected, ModelIDs: []string{"a", "b"}, DefaultModelID: "a"}, ""},
		{"new default", &ModelsInput{ModelIDs: ptr([]string{"b"}), DefaultModelID: ptr("b")}, "", AgentModels{Access: ModelAccessSelected, ModelIDs: []string{"b"}, DefaultModelID: "b"}, ""},
		{"legacy conflicts", &ModelsInput{DefaultModelID: ptr("a")}, "b", AgentModels{}, ReasonConflictingFields},
		{"bad access", &ModelsInput{Access: ptr(ModelAccess("x"))}, "", AgentModels{}, "must be all or selected"},
		{"ids with all", &ModelsInput{Access: &all, ModelIDs: ptr([]string{"a"})}, "", AgentModels{}, ReasonModelIDsNotAllowedForAll},
		{"empty selection", &ModelsInput{Access: &sel, ModelIDs: ptr([]string{})}, "", AgentModels{}, ReasonModelsSelectionEmpty},
		{"default drops out", &ModelsInput{ModelIDs: ptr([]string{"b"})}, "", AgentModels{}, ReasonDefaultModelNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cur.ApplyModels(tc.in, tc.legacy, sets())
			if tc.wantReason != "" {
				if _, r := fieldReason(err); r != tc.wantReason {
					t.Fatalf("reason %q (%v), want %q", r, err, tc.wantReason)
				}
				return
			}
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	// all access on the current value stays all and drops stale ids.
	allCur := AgentModels{Access: ModelAccessAll, ModelIDs: []string{"zz"}, DefaultModelID: "a"}
	if got, err := allCur.ApplyModels(nil, "", sets()); err != nil || len(got.ModelIDs) != 0 {
		t.Errorf("all: %+v %v", got, err)
	}
}

func TestModelsEqualEffectiveAndAudit(t *testing.T) {
	a := AgentModels{Access: ModelAccessSelected, ModelIDs: []string{"b", "a"}, DefaultModelID: "a"}
	b := AgentModels{Access: ModelAccessSelected, ModelIDs: []string{"a", "b"}, DefaultModelID: "a"}
	if !a.Equal(b) || a.Equal(AgentModels{Access: ModelAccessAll, DefaultModelID: "a"}) {
		t.Error("Equal")
	}
	got := AgentModels{Access: ModelAccessAll, DefaultModelID: "b"}.ComputeEffective([]string{"a", "b", "c"}, []string{"a", "b"})
	if !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("effective %v", got)
	}
	got = a.ComputeEffective([]string{"a", "b", "c"}, []string{"a", "b", "c"})
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("selected effective %v", got)
	}
	if gone, empty := a.ModelUnavailable([]string{"b"}); !gone || empty {
		t.Error("default gone")
	}
	if gone, empty := a.ModelUnavailable(nil); !gone || !empty {
		t.Error("empty")
	}
	if gone, _ := a.ModelUnavailable([]string{"a"}); gone {
		t.Error("default present")
	}
	if au := (AgentModels{Access: ModelAccessAll}).Audit(); au["access"] != "all" || len(au["modelIds"].([]string)) != 0 {
		t.Errorf("audit %v", au)
	}
}

func TestValidateTeamModelAccess(t *testing.T) {
	svc := []string{"a", "b"}
	many := make([]string, MaxSelectedModels+1)
	tests := []struct {
		name       string
		access     ModelAccess
		ids        []string
		want       ModelAccess
		wantReason string
	}{
		{"all", ModelAccessAll, nil, ModelAccessAll, ""},
		{"ids only is selected", "", []string{"a"}, ModelAccessSelected, ""},
		{"selected", ModelAccessSelected, []string{"a", "b"}, ModelAccessSelected, ""},
		{"nothing", "", nil, "", "is required"},
		{"all with ids", ModelAccessAll, []string{"a"}, "", ReasonModelIDsNotAllowedForAll},
		{"empty selection", ModelAccessSelected, []string{}, "", ReasonModelsSelectionEmpty},
		{"too many", ModelAccessSelected, many, "", "at most 50 models"},
		{"twice", ModelAccessSelected, []string{"a", "a"}, "", "is listed twice"},
		{"not enabled", ModelAccessSelected, []string{"z"}, "", ReasonModelNotEnabledForService},
		{"bad access", "x", []string{"a"}, "", "must be all or selected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateTeamModelAccess(tc.access, tc.ids, svc)
			if tc.wantReason != "" {
				if _, r := fieldReason(err); r != tc.wantReason {
					t.Fatalf("reason %q (%v)", r, err)
				}
				return
			}
			if err != nil || got.Access != tc.want {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestCapabilityValidation(t *testing.T) {
	if !HasOrchestrationCapability([]string{"orchestration"}) || !HasOrchestrationCapability([]string{"paperclip"}) || HasOrchestrationCapability([]string{"tool:buzz"}) {
		t.Error("HasOrchestrationCapability")
	}
	tests := []struct {
		name string
		caps []string
		want int
	}{
		{"none", nil, 0},
		{"orchestration and tool", []string{"orchestration", "tool:buzz"}, 0},
		{"deprecated alone", []string{"paperclip"}, 0},
		{"unknown", []string{"x"}, 1},
		{"repeat", []string{"orchestration", "orchestration"}, 1},
		{"two tools", []string{"orchestration", "tool:buzz", "tool:paperclip"}, 1},
		{"tool without orchestration", []string{"tool:buzz"}, 1},
	}
	for _, tc := range tests {
		if got := len(validateCapabilities(tc.caps)); got != tc.want {
			t.Errorf("%s: %d errors, want %d", tc.name, got, tc.want)
		}
	}
	for _, v := range []string{"", "orchestration", "tool:paperclip", "tool:buzz", "paperclip"} {
		if ValidateCapabilityFilter(v) != nil {
			t.Errorf("filter %q refused", v)
		}
	}
	if ValidateCapabilityFilter("x") == nil {
		t.Error("filter x accepted")
	}
}

func routeSets() ModelSets {
	s := sets()
	s.Routes = map[string][]string{"openai": {"gpt-x", "gpt-y"}, "local": {"llama"}}
	return s
}

func ptrS(s string) *string { return &s }

// MEG-API-9: route access, routes and a route default on create (v1.5.0 §4.13).
func TestResolveRoutes(t *testing.T) {
	sel := "selected"
	m, err := ResolveNewModels(&ModelsInput{DefaultModelID: ptrS("openai/gpt-x"), Routes: &RoutesInput{Access: &sel, Routes: &[]string{"openai"}}}, "", routeSets())
	if err != nil || m.DefaultRoute != "openai" || m.DefaultModelID != "gpt-x" || m.DefaultRef() != "openai/gpt-x" || m.RouteAccess != ModelAccessSelected {
		t.Fatalf("%+v %v", m, err)
	}
	// defaultRoute with the model name.
	m, err = ResolveNewModels(&ModelsInput{DefaultModelID: ptrS("llama"), Routes: &RoutesInput{DefaultRoute: ptrS("local")}}, "", routeSets())
	if err != nil || m.DefaultRoute != "local" || m.RouteAccess != ModelAccessAll || len(m.Routes) != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	// Routes alone read as a selection; the setting default is a route model.
	s := routeSets()
	s.SettingDefault = "local/llama"
	m, err = ResolveNewModels(&ModelsInput{Routes: &RoutesInput{Routes: &[]string{"local"}}}, "", s)
	if err != nil || m.RouteAccess != ModelAccessSelected || m.DefaultRef() != "local/llama" {
		t.Fatalf("%+v %v", m, err)
	}
	// Without routes, a route setting default that is outside the selection falls back to a Bedrock model.
	m, err = ResolveNewModels(&ModelsInput{Routes: &RoutesInput{Access: &sel, Routes: &[]string{"openai"}}}, "", s)
	if err != nil || m.DefaultRoute != "" || m.DefaultModelID != "a" {
		t.Fatalf("fallback %+v %v", m, err)
	}
	all := "all"
	bad := "x"
	for name, c := range map[string]struct {
		in            *ModelsInput
		field, reason string
	}{
		"route not in team":    {&ModelsInput{Routes: &RoutesInput{Routes: &[]string{"nope"}}}, "models.routes[0]", ReasonAgentRoutesTeam},
		"twice":                {&ModelsInput{Routes: &RoutesInput{Routes: &[]string{"local", "local"}}}, "models.routes[1]", "is listed twice"},
		"routes with all":      {&ModelsInput{Routes: &RoutesInput{Access: &all, Routes: &[]string{"local"}}}, "models.routes", ReasonRoutesNotAllowedForAll},
		"bad access":           {&ModelsInput{Routes: &RoutesInput{Access: &bad}}, "models.routeAccess", "must be all or selected"},
		"default not on route": {&ModelsInput{DefaultModelID: ptrS("local/gpt-x")}, "models.defaultModelId", ReasonAgentsDefaultRouteAllowed},
		"default route not selected": {&ModelsInput{DefaultModelID: ptrS("openai/gpt-x"), Routes: &RoutesInput{Access: &sel, Routes: &[]string{"local"}}},
			"models.defaultModelId", ReasonAgentsDefaultRouteAllowed},
		"conflict":            {&ModelsInput{DefaultModelID: ptrS("openai/gpt-x"), Routes: &RoutesInput{DefaultRoute: ptrS("local")}}, "models.defaultRoute", ReasonConflictingFields},
		"route without model": {&ModelsInput{Routes: &RoutesInput{DefaultRoute: ptrS("local")}}, "models.defaultModelId", "required"},
	} {
		_, err := ResolveNewModels(c.in, "", routeSets())
		if f, r := fieldReason(err); f != c.field || r != c.reason {
			t.Errorf("%s: %s %s (%v)", name, f, r, err)
		}
	}
	many := make([]string, MaxSelectedRoutes+1)
	for i := range many {
		many[i] = "r" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := ResolveNewModels(&ModelsInput{Routes: &RoutesInput{Routes: &many}}, "", routeSets()); err == nil {
		t.Error("51 routes accepted")
	}
	// Routes unknown (no route set): a route default is refused.
	if _, err := ResolveNewModels(&ModelsInput{DefaultModelID: ptrS("openai/gpt-x")}, "", sets()); err == nil {
		t.Error("route default without a route set")
	}
}

func TestApplyRoutes(t *testing.T) {
	cur := AgentModels{Access: ModelAccessAll, ModelIDs: []string{}, DefaultModelID: "a"}
	sel, all := "selected", "all"
	next, err := cur.ApplyModels(&ModelsInput{DefaultModelID: ptrS("openai/gpt-y"), Routes: &RoutesInput{Access: &sel, Routes: &[]string{"openai", "local"}}}, "", routeSets())
	if err != nil || next.DefaultRef() != "openai/gpt-y" || len(next.Routes) != 2 || next.Equal(cur) {
		t.Fatalf("%+v %v", next, err)
	}
	// Back to Bedrock: a Bedrock default clears the route; all clears the routes.
	back, err := next.ApplyModels(&ModelsInput{DefaultModelID: ptrS("b"), Routes: &RoutesInput{Access: &all, DefaultRouteNull: true}}, "", routeSets())
	if err != nil || back.DefaultRoute != "" || back.DefaultModelID != "b" || len(back.Routes) != 0 || back.RouteAccess != ModelAccessAll {
		t.Fatalf("back %+v %v", back, err)
	}
	// Clearing defaultRoute needs a new default.
	if _, err := next.ApplyModels(&ModelsInput{Routes: &RoutesInput{DefaultRouteNull: true}}, "", routeSets()); err == nil {
		t.Error("cleared route without a default")
	}
	// defaultRoute alone moves the default to the same model name on another route.
	moved, err := next.ApplyModels(&ModelsInput{Routes: &RoutesInput{DefaultRoute: ptrS("openai")}}, "", routeSets())
	if err != nil || moved.DefaultRoute != "openai" {
		t.Fatalf("moved %+v %v", moved, err)
	}
	// Narrowing the routes so the default's route is gone is refused.
	if _, err := next.ApplyModels(&ModelsInput{Routes: &RoutesInput{Routes: &[]string{"local"}}}, "", routeSets()); err == nil {
		t.Error("default route dropped")
	}
	// The legacy modelId alias can name a route model.
	leg, err := cur.ApplyModels(nil, "local/llama", routeSets())
	if err != nil || leg.DefaultRef() != "local/llama" {
		t.Fatalf("legacy %+v %v", leg, err)
	}
}

func TestRouteModelUnavailable(t *testing.T) {
	m := AgentModels{DefaultRoute: "openai", DefaultModelID: "gpt-x", EffectiveRoutes: []string{"openai"}}
	if gone, empty := m.ModelUnavailable(nil); gone || empty {
		t.Error("route default is available")
	}
	m.EffectiveRoutes = nil
	if gone, empty := m.ModelUnavailable(nil); !gone || !empty {
		t.Error("route default is gone")
	}
}

func TestRouteEnv(t *testing.T) {
	env := RouteEnv([]string{"openai-prod", "local"}, "openai-prod", "gpt-x", true)
	want := []EnvVar{{"AFS_LLM_ROUTES", "openai-prod,local"}, {"AFS_LLM_OPENAI_PROD_BASE_URL", "$(AFS_GATEWAY_URL)/r/openai-prod/v1"},
		{"AFS_LLM_LOCAL_BASE_URL", "$(AFS_GATEWAY_URL)/r/local/v1"}, {"AFS_LLM_BEDROCK_BASE_URL", "$(AFS_GATEWAY_URL)/r/bedrock/v1"},
		{"AFS_LLM_DEFAULT_ROUTE", "openai-prod"}, {"AFS_LLM_DEFAULT_MODEL", "gpt-x"}, {"OPENAI_BASE_URL", "$(AFS_GATEWAY_URL)/openai/v1"},
		{"OPENAI_API_KEY", "afs-gateway"}}
	if !slices.Equal(env, want) {
		t.Fatalf("%v", env)
	}
	if len(RouteEnv(nil, "", "", false)) != 0 {
		t.Error("no routes, no Bedrock: no variables")
	}
	if got := OrderRoutes([]string{"b", "a", "c", "a"}, "c"); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Errorf("order %v", got)
	}
}
