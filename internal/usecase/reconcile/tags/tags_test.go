package tags

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func TestEffectiveAlwaysCarriesTheSystemTags(t *testing.T) {
	got, err := Effective(Subject{TeamID: "team_1", TeamSlug: "pay", AgentID: "agt_1", AgentName: "rev"},
		map[string]string{"environment": "prod", "afs-team-id": "forged", "AFS-agent-id": "x", "aws:cloudformation:stack": "s", "empty": ""})
	if err != nil {
		t.Fatal(err)
	}
	want := Set{KeyTeamID: "team_1", KeyTeamSlug: "pay", KeyAgentID: "agt_1", KeyAgentName: "rev", "environment": "prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	// A team set built from nothing still carries afs-team-id (never removed).
	only, err := Effective(Subject{TeamID: "team_1"}, nil)
	if err != nil || only[KeyTeamID] != "team_1" || len(only) != 1 {
		t.Fatalf("%v %v", only, err)
	}
}

func TestEffectiveRefusesAboveTheLimit(t *testing.T) {
	mk := func(n int) map[string]string {
		m := map[string]string{}
		for i := range n {
			m[fmt.Sprintf("k%02d", i)] = "v"
		}
		return m
	}
	// team id + slug + agent id + agent name = 4 system keys.
	s := Subject{TeamID: "team_1", TeamSlug: "pay", AgentID: "agt_1", AgentName: "rev"}
	if _, err := Effective(s, mk(36)); err != nil {
		t.Fatalf("exactly %d keys must pass: %v", MaxKeys, err)
	}
	_, err := Effective(s, mk(37))
	var le *LimitError
	if !errors.As(err, &le) || le.Keys != 41 || le.Max != 40 || le.Subject != "agt_1" {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "limit is 40") || le.ErrorCode() != ErrorCode {
		t.Fatalf("the error must name the limit: %v", err)
	}
}

func TestAppliedRecordAndStale(t *testing.T) {
	first := Set{KeyTeamID: "team_1", "environment": "prod", "cost-center": "42"}.WithApplied()
	if first[KeyApplied] != "cost-center,environment" {
		t.Fatalf("record: %q", first[KeyApplied])
	}
	// The team drops cost-center and a service tag is added.
	next := Set{KeyTeamID: "team_1", "environment": "prod", "business-unit": "x"}.WithApplied()
	have := map[string]string{}
	for k, v := range first {
		have[k] = v
	}
	have["created-by-a-human"] = "keep"
	have[KeyManagedBy] = "afs"
	have[KeyOrphan] = "true"
	have[KeyAgentID] = "agt_1" // an afs- key AFS owns, no longer wanted
	if got, want := Stale(have, next), []string{KeyAgentID, "cost-center"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stale = %v, want %v", got, want)
	}
	if got := Diff(have, next); !reflect.DeepEqual(got, map[string]string{"business-unit": "x", KeyApplied: "business-unit,environment"}) {
		t.Fatalf("diff = %v", got)
	}
	// Nothing recorded, nothing wanted away but afs- keys: foreign keys survive.
	if got := Stale(map[string]string{"human": "x"}, next); len(got) != 0 {
		t.Fatalf("foreign key removed: %v", got)
	}
}

func TestAppliedRecordIsLeftOffWhenTooLong(t *testing.T) {
	s := Set{KeyTeamID: "t"}
	for i := range 30 {
		s[fmt.Sprintf("a-rather-long-tag-key-number-%02d", i)] = "v"
	}
	if _, ok := s.WithApplied()[KeyApplied]; ok {
		t.Fatal("a record longer than a tag value must be left off")
	}
}

func TestRoutesReachTheTeamKind(t *testing.T) {
	got := map[string]engine.Route{}
	for _, r := range Routes() {
		got[r.Table] = r
	}
	if r := got["team_tags"]; len(r.Targets) != 1 || r.Targets[0].Kind != engine.KindTeam || r.Targets[0].Self {
		t.Errorf("team_tags = %+v", r)
	}
	if r := got["service_tags"]; len(r.FanOut) != 1 || r.FanOut[0] != engine.KindTeam {
		t.Errorf("service_tags = %+v", r)
	}
}
