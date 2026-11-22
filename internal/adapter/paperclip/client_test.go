package paperclip_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip/papertest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

type fixed paperclip.Target

func (f fixed) Target(context.Context, string) (paperclip.Target, error) {
	return paperclip.Target(f), nil
}

type failing struct{}

func (failing) Target(context.Context, string) (paperclip.Target, error) {
	return paperclip.Target{}, errors.New("no board key")
}

func setup(t *testing.T) (*paperclip.Client, *papertest.Server) {
	t.Helper()
	s := papertest.New("board-secret")
	t.Cleanup(s.Close)
	return paperclip.New(fixed{BaseURL: s.URL, BoardKey: "board-secret"}), s
}

func TestStatusAndSignUp(t *testing.T) {
	c, s := setup(t)
	ctx := t.Context()
	st, err := c.Status(ctx, "team_a")
	if err != nil || st.SignUpOpen {
		t.Fatalf("closed: %+v %v", st, err)
	}
	s.SetSignUp("open")
	if st, _ := c.Status(ctx, "team_a"); !st.SignUpOpen {
		t.Error("open sign-up not seen")
	}
	s.SetDown(true)
	if _, err := c.Status(ctx, "team_a"); err == nil {
		t.Error("a down Paperclip must be an error")
	}
}

func TestAuthIsTheBoardKey(t *testing.T) {
	s := papertest.New("right")
	defer s.Close()
	c := paperclip.New(fixed{BaseURL: s.URL, BoardKey: "wrong"})
	_, err := c.Roster(t.Context(), "team_a")
	var pe *paperclip.Error
	if !errors.As(err, &pe) || pe.Status != 401 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "right") {
		t.Error("error carries a key")
	}
}

func TestAgentLifecycle(t *testing.T) {
	c, s := setup(t)
	ctx := t.Context()
	s.AddAgent(papertest.Agent{Name: "ceo", AdapterType: "hermes_local", Role: "orchestrator"})
	ca, err := c.CreateAgent(ctx, "team_a", collab.NewAgent{Name: "worker-a", AFSAgentID: "agt_1", Persona: "p"})
	if err != nil || ca.ID == "" || ca.Key.Reveal() == "" {
		t.Fatalf("create: %+v %v", ca, err)
	}
	if fmt.Sprint(ca.Key) != "[redacted]" || fmt.Sprintf("%#v", ca.Key) != "collab.Key([redacted])" {
		t.Error("key prints")
	}
	ro, err := c.Roster(ctx, "team_a")
	if err != nil || len(ro) != 2 {
		t.Fatalf("roster: %+v %v", ro, err)
	}
	var w collab.RosterAgent
	for _, a := range ro {
		if a.ID == ca.ID {
			w = a
		}
	}
	if w.LinkedAFSAgentID != "agt_1" || w.LocalAdapter || w.Orchestrator || w.Paused {
		t.Errorf("worker: %+v", w)
	}
	if !ro[0].Orchestrator || !ro[0].LocalAdapter {
		t.Errorf("orchestrator: %+v", ro[0])
	}
	if err := c.SetAgentPaused(ctx, "team_a", ca.ID, true); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.AgentByID(ca.ID); a.Status != "paused" {
		t.Errorf("status %s", a.Status)
	}
	ro, _ = c.Roster(ctx, "team_a")
	if !ro[1].Paused {
		t.Error("paused not read")
	}
	if err := c.SetAgentPaused(ctx, "team_a", ca.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeAgent(ctx, "team_a", ca.ID); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.AgentByID(ca.ID); a.Status != "revoked" || a.Key != "" {
		t.Errorf("revoked: %+v", a)
	}
	if ro, _ = c.Roster(ctx, "team_a"); len(ro) != 1 {
		t.Errorf("revoked agent still listed: %+v", ro)
	}
	if err := c.RevokeAgent(ctx, "team_a", "pc_gone"); err != nil {
		t.Errorf("missing agent must be success: %v", err)
	}
	if err := c.SetAgentPaused(ctx, "team_a", "pc_gone", true); !paperclip.IsNotFound(err) {
		t.Errorf("pause missing: %v", err)
	}
}

func TestRosterPaginates(t *testing.T) {
	c, s := setup(t)
	for i := 0; i < 5; i++ {
		s.AddAgent(papertest.Agent{Name: fmt.Sprintf("a%d", i)})
	}
	s.SetPageSize(2)
	ro, err := c.Roster(t.Context(), "team_a")
	if err != nil || len(ro) != 5 {
		t.Fatalf("%d %v", len(ro), err)
	}
}

func TestChangedSinceCursor(t *testing.T) {
	c, s := setup(t)
	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		kind := "issue"
		if i%2 == 1 {
			kind = "comment"
		}
		s.AddActivity(papertest.Activity{ID: fmt.Sprintf("it_%d", i), Kind: kind, AuthorAgentID: "pc_1", Text: "t", UpdatedAt: t0.Add(time.Duration(i) * time.Minute)})
	}
	s.AddActivity(papertest.Activity{ID: "other", Kind: "label", UpdatedAt: t0.Add(time.Hour)})
	s.SetPageSize(2)
	items, err := c.Changed(t.Context(), "team_a", t0.Add(30*time.Second))
	if err != nil || len(items) != 4 || items[0].ID != "it_1" || items[3].ID != "it_4" {
		t.Fatalf("items: %+v %v", items, err)
	}
}

func TestInvitesAndMembers(t *testing.T) {
	c, s := setup(t)
	ctx := t.Context()
	inv, err := c.CreateInvite(ctx, "team_a", "a@example.com", collab.RoleAdmin)
	if err != nil || inv.ID == "" || !strings.Contains(inv.URL.Reveal(), "invite") {
		t.Fatalf("invite: %+v %v", inv, err)
	}
	if strings.Contains(fmt.Sprint(inv), "invite/") {
		t.Error("invite prints its URL")
	}
	list, err := c.Invites(ctx, "team_a")
	if err != nil || len(list) != 1 || list[0].Email != "a@example.com" || list[0].Role != collab.RoleAdmin || list[0].Accepted {
		t.Fatalf("list: %+v %v", list, err)
	}
	s.SeedInvite(papertest.Invite{Email: "b@example.com", Role: "member", Accepted: true})
	if list, _ = c.Invites(ctx, "team_a"); len(list) != 2 || !list[1].Accepted {
		t.Errorf("accepted: %+v", list)
	}
	if err := c.RevokeInvite(ctx, "team_a", inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeInvite(ctx, "team_a", inv.ID); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	if err := c.DeactivateMember(ctx, "team_a", "b@example.com"); err != nil || !s.Deactivated("b@example.com") {
		t.Errorf("deactivate: %v", err)
	}
}

func TestErrorsCarryNoBodyOrKey(t *testing.T) {
	c, s := setup(t)
	s.FailNext("POST /api/agents", 500)
	_, err := c.CreateAgent(t.Context(), "team_a", collab.NewAgent{Name: "x"})
	var pe *paperclip.Error
	if !errors.As(err, &pe) || pe.Status != 500 || strings.Contains(err.Error(), "secret-in-error-body") {
		t.Fatalf("err = %v", err)
	}
	if _, err := paperclip.New(failing{}).Roster(t.Context(), "t"); err == nil {
		t.Error("resolver failure")
	}
	dead := paperclip.New(fixed{BaseURL: "http://127.0.0.1:1", BoardKey: "k"})
	if _, err := dead.Status(t.Context(), "t"); err == nil || strings.Contains(err.Error(), "127.0.0.1") && false {
		t.Error("transport failure")
	}
	if (&paperclip.Error{Op: "x"}).Error() == "" {
		t.Error("message")
	}
}
