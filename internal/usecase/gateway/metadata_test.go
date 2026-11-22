package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// M4-B2 / GW-5: the D27 order is system (team, agent or pool), service tags whoever set the value, then team
// tags in key order; 16 pairs at most; what did not fit is returned in order.
func TestBuildMetadataFollowsD27(t *testing.T) {
	tags := []gateway.Tag{
		{Key: "zeta", Value: "z", Source: "team"},
		{Key: "env", Value: "prod", Source: "team", Service: true}, // required key the team filled in
		{Key: "cost-center", Value: "cc", Source: "service_default", Service: true},
		{Key: "afs-team-id", Value: "team_1", Source: "system"},
		{Key: "alpha", Value: "a", Source: "team"},
		{Key: "fixed", Value: "f", Source: "service", Service: true},
		{Key: "afs-agent-id", Value: "forged", Source: "team"}, // a team tag cannot displace a system key
		{Key: "blank", Value: "", Source: "team"},
	}
	tests := []struct {
		name    string
		caller  gateway.Caller
		tags    []gateway.Tag
		want    map[string]string
		dropped []string
	}{
		{"agent", gateway.Caller{TeamID: "team_1", AgentID: "agt_1"}, tags,
			map[string]string{"afs-team-id": "team_1", "afs-agent-id": "agt_1", "cost-center": "cc", "env": "prod", "fixed": "f", "alpha": "a", "zeta": "z"}, nil},
		{"pool", gateway.Caller{TeamID: "team_1", PoolID: "pool_1"}, tags[:1],
			map[string]string{"afs-team-id": "team_1", "afs-pool-id": "pool_1", "zeta": "z"}, nil},
		{"no tags", gateway.Caller{TeamID: "team_1", AgentID: "agt_1"}, nil,
			map[string]string{"afs-team-id": "team_1", "afs-agent-id": "agt_1"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := gateway.BuildMetadata(tc.caller, tc.tags)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) || !slices.Equal(dropped, tc.dropped) {
				t.Errorf("meta = %v dropped = %v", got, dropped)
			}
		})
	}
}

func TestBuildMetadataDropsInOrderAtSixteen(t *testing.T) {
	var tags []gateway.Tag
	for i := range 6 {
		tags = append(tags, gateway.Tag{Key: fmt.Sprintf("svc-%d", i), Value: "s", Source: "service", Service: true})
	}
	for i := range 14 {
		tags = append(tags, gateway.Tag{Key: fmt.Sprintf("own-%02d", i), Value: "v", Source: "team"})
	}
	meta, dropped := gateway.BuildMetadata(gateway.Caller{TeamID: "team_1", AgentID: "agt_1"}, tags)
	if len(meta) != gateway.MaxRequestMetadata {
		t.Fatalf("pairs = %d", len(meta))
	}
	// 2 system + 6 service + 8 team keep; own-08 .. own-13 do not.
	if meta["own-07"] == "" || meta["own-08"] != "" || !slices.Equal(dropped, []string{"own-08", "own-09", "own-10", "own-11", "own-12", "own-13"}) {
		t.Errorf("meta = %v dropped = %v", meta, dropped)
	}
}

// The keys the gateway drops for an agent call are the ones GetTeamTags lists (teams.Service keeps one pair
// for the agent id): both read the same D27 order.
func TestGatewayAndTeamTagsAgreeOnDroppedKeys(t *testing.T) {
	val := func(s string) *string { return &s }
	svc := []domain.ServiceTag{
		{Key: "env", Mode: domain.ServiceTagModeRequired, DefaultValue: val("dev")},
		{Key: "owner", Mode: domain.ServiceTagModeFixed, Value: val("platform")},
	}
	var own []domain.TagValue
	for i := range 22 {
		own = append(own, domain.TagValue{Key: fmt.Sprintf("k%02d", i), Value: "v"})
	}
	own = append(own, domain.TagValue{Key: "env", Value: "prod"})
	set := domain.EffectiveTagSet("team_1", svc, own)
	_, want := set.BedrockMetadata(domain.MaxRequestMetadataPairs - 1)

	isSvc := map[string]bool{"env": true, "owner": true}
	var tags []gateway.Tag
	for _, tg := range set.Tags {
		tags = append(tags, gateway.Tag{Key: tg.Key, Value: tg.Value, Source: string(tg.Source), Service: isSvc[tg.Key]})
	}
	slices.Reverse(tags) // the store's row order must not matter
	_, got := gateway.BuildMetadata(gateway.Caller{TeamID: "team_1", AgentID: "agt_1"}, tags)
	if !slices.Equal(got, want) || len(want) == 0 {
		t.Errorf("gateway dropped %v, GetTeamTags lists %v", got, want)
	}
}

// Dropped keys are recorded: a warning when a team's dropped set appears or changes, not on every call.
func TestDroppedKeysAreLoggedOnChange(t *testing.T) {
	var buf bytes.Buffer
	r := newRig(t)
	r.svc = gateway.NewService(gateway.Deps{Pods: r.dir, Tokens: r.tokens, Store: r.store, Spend: r.spend, Thresholds: r.thr, Usage: r.usage},
		gateway.Config{Partition: "aws", AccountID: "111122223333", Log: slog.New(slog.NewTextHandler(&buf, nil)), Now: func() time.Time { return now }})
	var rows []gateway.Tag
	for i := range 20 {
		rows = append(rows, gateway.Tag{Key: fmt.Sprintf("k%02d", i), Value: "v", Source: "team"})
	}
	r.store.TagRows[teamA] = rows
	call := func() map[string]string {
		var md map[string]string
		if err := r.svc.Do(t.Context(), req("10.0.0.1"), func(_ context.Context, a gateway.Authorized) (gateway.Result, error) {
			md = a.Metadata
			return ok(1, 1), nil
		}); err != nil {
			t.Fatal(err)
		}
		return md
	}
	md := call()
	call()
	if len(md) != 16 || strings.Count(buf.String(), "requestMetadata keys dropped") != 1 || !strings.Contains(buf.String(), "k14") {
		t.Errorf("pairs %d, log:\n%s", len(md), buf.String())
	}
	r.store.TagRows[teamA] = rows[:3]
	call()
	r.store.TagRows[teamA] = rows
	call()
	if strings.Count(buf.String(), "requestMetadata keys dropped") != 2 {
		t.Errorf("a dropped set that reappears is logged again:\n%s", buf.String())
	}
}
