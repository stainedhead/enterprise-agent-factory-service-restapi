package domain

import (
	"strings"
	"testing"
)

func TestValidateEndpoints(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		ok   bool
	}{
		{"host", []string{"github.com"}, true},
		{"wildcard and port", []string{"*.atlassian.net", "api.example.com:8443"}, true},
		{"empty", nil, false},
		{"upper case", []string{"GitHub.com"}, false},
		{"no dot", []string{"localhost"}, false},
		{"bare wildcard", []string{"*"}, false},
		{"wildcard mid", []string{"a.*.com"}, false},
		{"port zero", []string{"a.example.com:0"}, false},
		{"port too big", []string{"a.example.com:65536"}, false},
		{"port leading zero", []string{"a.example.com:080"}, false},
		{"port text", []string{"a.example.com:http"}, false},
		{"duplicate", []string{"a.example.com", "a.example.com"}, false},
		{"scheme", []string{"https://a.example.com"}, false},
		{"too many", make([]string, 51), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEndpoints("endpoints", tc.in)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v", err)
			}
			if err != nil && KindOf(err) != KindValidation {
				t.Errorf("kind = %v", KindOf(err))
			}
		})
	}
}

func goodDeclare() DeclareSystemParams {
	return DeclareSystemParams{Slug: "internal-wiki", Name: "Wiki", Category: ExtOther, Endpoints: []string{"wiki.example.com"},
		Owner: "platform", Purpose: "docs lookups", DocsURL: "https://wiki.example.com/help"}
}

func TestNewTeamSystem(t *testing.T) {
	s, err := NewTeamSystem("team_1", goodDeclare())
	if err != nil || s.TeamID != "team_1" || !s.Enabled || s.Builtin || !s.TeamDeclared() || s.Unmetered() {
		t.Fatalf("system = %+v, %v", s, err)
	}
	for name, mod := range map[string]func(*DeclareSystemParams){
		"slug":      func(p *DeclareSystemParams) { p.Slug = "X" },
		"name":      func(p *DeclareSystemParams) { p.Name = "" },
		"category":  func(p *DeclareSystemParams) { p.Category = "weird" },
		"owner":     func(p *DeclareSystemParams) { p.Owner = " " },
		"purpose":   func(p *DeclareSystemParams) { p.Purpose = "" },
		"endpoints": func(p *DeclareSystemParams) { p.Endpoints = nil },
		"docsUrl":   func(p *DeclareSystemParams) { p.DocsURL = "http://x.example.com" },
		"descr":     func(p *DeclareSystemParams) { p.Description = strings.Repeat("x", 2001) },
	} {
		p := goodDeclare()
		mod(&p)
		if _, err := NewTeamSystem("team_1", p); KindOf(err) != KindValidation || !strings.Contains(strings.Join(fieldsOf(err), ","), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNewCatalogSystem(t *testing.T) {
	p := NewCatalogSystemParams{Slug: "openai-2", Name: "OpenAI 2", Category: ExtLLM, Endpoints: []string{"api.example.com"}, Enabled: ptr(false)}
	s, err := NewCatalogSystem(p)
	if err != nil || s.Enabled || s.TeamDeclared() || s.Builtin || !s.Unmetered() {
		t.Fatalf("system = %+v, %v", s, err)
	}
	p.Enabled = nil
	if s, _ = NewCatalogSystem(p); !s.Enabled {
		t.Error("enabled should default to true")
	}
	p.Slug = "_"
	if _, err := NewCatalogSystem(p); KindOf(err) != KindValidation {
		t.Errorf("bad slug = %v", err)
	}
}

func TestSystemApply(t *testing.T) {
	team := ExternalSystem{TeamID: "team_1", Slug: "x-sys", Name: "X", Owner: "o", Purpose: "p", Endpoints: []string{"a.example.com"}, Enabled: true}
	cat := ExternalSystem{Slug: "x-sys", Name: "X", Endpoints: []string{"a.example.com"}, Enabled: true}
	eps := []string{"b.example.com:443"}
	tests := []struct {
		name string
		base ExternalSystem
		u    SystemUpdate
		bad  bool
		want func(ExternalSystem) bool
	}{
		{"team endpoints and owner", team, SystemUpdate{Endpoints: &eps, Owner: ptr(" new "), Purpose: ptr("q")}, false,
			func(s ExternalSystem) bool {
				return s.Endpoints[0] == "b.example.com:443" && s.Owner == "new" && s.Purpose == "q"
			}},
		{"catalog switch off", cat, SystemUpdate{Enabled: ptr(false), Name: ptr("N"), Description: ptr("d"), DocsURL: ptr("https://d.example.com")}, false,
			func(s ExternalSystem) bool {
				return !s.Enabled && s.Name == "N" && s.Description == "d" && s.DocsURL == "https://d.example.com"
			}},
		{"empty", team, SystemUpdate{}, true, nil},
		{"team owner blank", team, SystemUpdate{Owner: ptr("")}, true, nil},
		{"team purpose blank", team, SystemUpdate{Purpose: ptr("")}, true, nil},
		{"catalog owner refused", cat, SystemUpdate{Owner: ptr("x")}, true, nil},
		{"catalog purpose refused", cat, SystemUpdate{Purpose: ptr("x")}, true, nil},
		{"bad endpoints", team, SystemUpdate{Endpoints: &[]string{"NOPE"}}, true, nil},
		{"bad docs", team, SystemUpdate{DocsURL: ptr("ftp://x")}, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.base.Apply(tc.u)
			if (err != nil) != tc.bad {
				t.Fatalf("err = %v", err)
			}
			if err == nil && !tc.want(s) {
				t.Errorf("system = %+v", s)
			}
		})
	}
}

func TestAccessViews(t *testing.T) {
	for _, tc := range []struct {
		at       ExtSystemDisabledAt
		on, team bool
	}{
		{"", true, true}, {ExtSystemDisabledAtAgent, false, true}, {ExtSystemDisabledAtTeam, false, false}, {ExtSystemDisabledAtService, false, false},
	} {
		a := ExternalSystemAccess{DisabledAt: tc.at}
		if a.On() != tc.on || a.TeamOn() != tc.team {
			t.Errorf("%q: on=%v teamOn=%v", tc.at, a.On(), a.TeamOn())
		}
	}
}
