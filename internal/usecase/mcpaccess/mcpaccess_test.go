package mcpaccess_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess/mcpaccesstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team = "team_1"
	srvA = "mcp_A"
	srvB = "mcp_B"
)

func TestWriteAndFill(t *testing.T) {
	ctx := t.Context()
	st := mcpaccesstest.New()
	st.On[team] = []string{srvA}
	ids := &fakes.IDGen{}
	sel, full := "selected", domain.McpAgentModeFull
	tests := []struct {
		name string
		p    domain.McpAccessPatch
		want string
	}{
		{"server off", domain.McpAccessPatch{Access: &sel, ServerIDs: &[]string{srvB}}, domain.ReasonMcpServerNotEnabledForTeam},
		{"block on a server off", domain.McpAccessPatch{Blocks: &[]domain.McpAgentBlock{{ServerID: srvB, Name: "x", Effect: "block"}}},
			domain.ReasonMcpServerNotEnabledForTeam},
		{"allow rule", domain.McpAccessPatch{Blocks: &[]domain.McpAgentBlock{{ServerID: srvA, Name: "x", Effect: "allow"}}}, "only block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := mcpaccess.Write(ctx, nil, st, ids, team, mcpaccess.KindAgent, "agt_1", tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %s", err, tc.want)
			}
		})
	}
	before, after, err := mcpaccess.Write(ctx, nil, st, ids, team, mcpaccess.KindAgent, "agt_1", domain.McpAccessPatch{Access: &sel,
		ServerIDs: &[]string{srvA}, Mode: &full, Blocks: &[]domain.McpAgentBlock{{ServerID: srvA, Name: "delete_*", Effect: "block"}}})
	if err != nil || before["access"] != "all" || after["access"] != "selected" || after["mode"] != "full" {
		t.Fatalf("write = %v %v %v", before, after, err)
	}
	got, err := mcpaccess.Fill(ctx, nil, st, team, mcpaccess.KindAgent, []string{"agt_1", "agt_2"})
	if err != nil {
		t.Fatal(err)
	}
	if a := got["agt_1"]; a.Access != "selected" || a.Mode != full || len(a.Env) != 4 || len(a.Blocks) != 1 || a.Effective[0] != srvA {
		t.Errorf("agt_1 = %+v", a)
	}
	if b := got["agt_2"]; b.Access != "all" || b.Mode != domain.McpAgentModeSearch || len(b.Env) != 4 || b.ServerIDs == nil || b.Blocks == nil {
		t.Errorf("agt_2 = %+v", b)
	}
	if m, err := mcpaccess.Fill(ctx, nil, st, team, mcpaccess.KindAgent, nil); err != nil || len(m) != 0 {
		t.Errorf("no ids = %v %v", m, err)
	}
	st.FailPut = errors.New("down")
	if _, _, err := mcpaccess.Write(ctx, nil, st, ids, team, mcpaccess.KindAgent, "agt_1", domain.McpAccessPatch{}); err == nil {
		t.Error("a failed write was ignored")
	}
	if _, _, err := mcpaccess.Write(ctx, nil, st, badIDs{}, team, mcpaccess.KindAgent, "agt_1",
		domain.McpAccessPatch{Blocks: &[]domain.McpAgentBlock{{ServerID: srvA, Name: "x", Effect: "block"}}}); err == nil {
		t.Error("a failed id was ignored")
	}
	st.FailDefault = errors.New("down")
	if _, err := mcpaccess.Fill(ctx, nil, st, team, mcpaccess.KindAgent, []string{"agt_1"}); err == nil {
		t.Error("a failed default was ignored")
	}
	st.Fail = errors.New("down")
	if _, _, err := mcpaccess.Write(ctx, nil, st, ids, team, mcpaccess.KindAgent, "agt_1", domain.McpAccessPatch{}); err == nil {
		t.Error("a failed read was ignored")
	}
	if _, err := mcpaccess.Fill(ctx, nil, st, team, mcpaccess.KindAgent, []string{"agt_1"}); err == nil {
		t.Error("a failed fill was ignored")
	}
}

func TestMerge(t *testing.T) {
	sel, all := "selected", "all"
	cur := domain.AgentMcp{Access: "selected", ServerIDs: []string{srvA}, Mode: domain.McpAgentModeFull}
	if m := (domain.McpAccessPatch{Access: &all}).Merge(cur); m.Access != "all" || m.ServerIDs != nil || m.Mode != domain.McpAgentModeFull {
		t.Errorf("to all = %+v", m)
	}
	if m := (domain.McpAccessPatch{Access: &sel}).Merge(cur); len(m.ServerIDs) != 1 {
		t.Errorf("keep = %+v", m)
	}
	if m := (domain.McpAccessPatch{}).Merge(domain.AgentMcp{}); m.Access != "all" {
		t.Errorf("empty = %+v", m)
	}
}

type badIDs struct{}

func (badIDs) NewID(domain.IDPrefix) (domain.ID, error) {
	var id domain.ID
	return id, errors.New("no ids")
}
