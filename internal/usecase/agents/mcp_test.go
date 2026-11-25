package agents_test

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess/mcpaccesstest"
)

const srv = "mcp_01JD2Q3R4S5T6V7W8X9Y0Z0001"

// The mcp member of CreateAgent and UpdateAgent (contract §4.14): stored in the same transaction, in the audit
// record and the updated event, read back with the effective servers and the AFS_MCP_* names; a server the team
// has off is refused; without a store an mcp body is refused rather than ignored.
func TestAgentMcp(t *testing.T) {
	r := newRig()
	st := mcpaccesstest.New()
	st.On[team] = []string{srv}
	r.svc.Mcp = st
	sel, full := "selected", domain.McpAgentModeFull
	in := params("coder")
	in.Mcp = &domain.McpAccessPatch{Access: &sel, ServerIDs: &[]string{srv}}
	res, err := r.svc.Create(t.Context(), admin, team, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Agent
	if a.Mcp == nil || a.Mcp.Access != "selected" || a.Mcp.Mode != domain.McpAgentModeSearch || len(a.Mcp.Effective) != 1 || len(a.Mcp.Env) != 2 {
		t.Fatalf("created mcp = %+v", a.Mcp)
	}
	if after := r.audit.Entries[len(r.audit.Entries)-1].Record.After.(map[string]any); after["mcp"] == nil {
		t.Error("the audit record has no mcp")
	}
	got, err := r.svc.Update(t.Context(), admin, team, a.ID, a.Version, domain.AgentPatch{Mcp: &domain.McpAccessPatch{Mode: &full}})
	if err != nil || got.Mcp.Mode != full || len(got.Mcp.Env) != 3 || got.Mcp.Access != "selected" {
		t.Fatalf("update = %+v %v", got.Mcp, err)
	}
	ev := r.outbox.Events[len(r.outbox.Events)-1]
	if f, _ := ev.Detail.Data["fields"].([]string); len(f) != 1 || f[0] != "mcp" {
		t.Errorf("event fields = %v", ev.Detail.Data["fields"])
	}
	off := "mcp_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	if _, err := r.svc.Update(t.Context(), admin, team, a.ID, got.Version, domain.AgentPatch{Mcp: &domain.McpAccessPatch{ServerIDs: &[]string{off}}}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("a server that is off: %v", err)
	}
	list, _, err := r.svc.List(t.Context(), admin, team, "", acting.Page{Limit: 10})
	if err != nil || len(list) != 1 || list[0].Mcp == nil {
		t.Errorf("list = %+v %v", list, err)
	}
	r.svc.Mcp = nil
	if g, err := r.svc.Get(t.Context(), admin, team, a.ID); err != nil || g.Mcp != nil {
		t.Errorf("without a store: %+v %v", g.Mcp, err)
	}
	in.Name = "other"
	if _, err := r.svc.Create(t.Context(), admin, team, in, nil); !errors.Is(err, mcpaccess.ErrNoStore) {
		t.Errorf("without a store an mcp body = %v", err)
	}
}
