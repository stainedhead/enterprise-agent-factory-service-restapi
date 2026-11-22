package collab_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip/papertest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

type statesSrc struct{ states []collab.TeamState }

func (s statesSrc) States(context.Context) ([]collab.TeamState, error) { return s.states, nil }

func managedState() collab.TeamState {
	return collab.TeamState{TeamID: "team_a", AgentID: "agt_c", Mode: domain.RegistrationManaged, PodReady: true}
}

func client(t *testing.T) (*paperclip.Client, *papertest.Server) {
	t.Helper()
	srv := papertest.New("bk")
	t.Cleanup(srv.Close)
	return paperclip.New(target{BaseURL: srv.URL, BoardKey: "bk"}), srv
}

// COL-27: sign-up open is PaperclipReady=False with reason OpenSignUp.
func TestProbeReasons(t *testing.T) {
	c, srv := client(t)
	st := &stat{}
	p := &collab.Prober{Source: statesSrc{[]collab.TeamState{managedState(), {TeamID: "team_m", Mode: domain.RegistrationManual, PodReady: true}, {TeamID: "team_off", Mode: domain.RegistrationManaged}}},
		Paper: c, Status: st, Now: func() time.Time { return now0 }}
	if err := p.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.reports) != 1 || st.last().Status != domain.ConditionTrue || st.last().Reason != collab.ReasonHealthy {
		t.Fatalf("healthy: %+v", st.reports)
	}
	srv.SetSignUp("open")
	_ = p.Run(t.Context())
	if c := st.last(); c.Type != collab.ConditionPaperclipReady || c.Status != domain.ConditionFalse || c.Reason != collab.ReasonOpenSignUp || c.Message == "" {
		t.Errorf("open: %+v", c)
	}
	srv.SetDown(true)
	_ = p.Run(t.Context())
	if c := st.last(); c.Reason != collab.ReasonNotAnswering || c.Status != domain.ConditionFalse {
		t.Errorf("down: %+v", c)
	}
}

type mailbox struct {
	sent []struct{ to, subject, body string }
	err  error
}

func (m *mailbox) Send(_ context.Context, to []string, subject, body string) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, struct{ to, subject, body string }{strings.Join(to, ","), subject, body})
	return nil
}

type admins struct {
	email string
	err   error
}

func (a admins) CreatorEmail(context.Context, string, string) (string, error) { return a.email, a.err }

type boardKeys struct {
	cands    []collab.BoardKeyCandidate
	found    bool
	recorded int
}

func (b *boardKeys) Pending(context.Context) ([]collab.BoardKeyCandidate, error) { return b.cands, nil }
func (b *boardKeys) Find(context.Context, string, string) (string, bool, error) {
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/ns/agt_c/paperclip-board-api-key-AbCdEf", b.found, nil
}
func (b *boardKeys) Record(context.Context, string, string, string) error {
	b.recorded++
	b.cands[0].Recorded = true
	return nil
}

// COL-32: the first admin gets one email with the invite URL; the event and the logs never carry it.
func TestBootstrapInvitesTheFirstAdmin(t *testing.T) {
	c, srv := client(t)
	keys := &boardKeys{cands: []collab.BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c", Mode: domain.RegistrationManaged, PodReady: true}}, found: true}
	mb, ev := &mailbox{}, &evs{}
	var logs bytes.Buffer
	var evData []map[string]any
	pe := publishCapture{evs: ev, data: &evData}
	b := &collab.Bootstrap{Keys: keys, Events: pe, Paper: c, Mail: mb, Admins: admins{email: "boss@example.com"}, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := b.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(mb.sent) != 1 || mb.sent[0].to != "boss@example.com" || !strings.Contains(mb.sent[0].body, "https://paperclip.example/invite/") {
		t.Fatalf("mail: %+v", mb.sent)
	}
	inv := srv.Invites()
	if len(inv) != 1 || inv[0].Role != "admin" || inv[0].Email != "boss@example.com" {
		t.Fatalf("invites: %+v", inv)
	}
	if ev.count(domain.EventCollaborationBootstrapCompleted) != 1 {
		t.Fatalf("events: %v", ev.got)
	}
	for _, d := range evData {
		for _, v := range d {
			if s, ok := v.(string); ok && strings.Contains(s, "invite") {
				t.Error("the event carries the invite")
			}
		}
	}
	if strings.Contains(logs.String(), "invite/") {
		t.Error("the log carries the invite URL")
	}
	// Runs again: nothing more is sent, the event is not repeated.
	_ = b.Run(t.Context())
	// A restart (a fresh Bootstrap) finds the admin invite and neither mails nor emits.
	b2 := &collab.Bootstrap{Keys: keys, Events: pe, Paper: c, Mail: mb, Admins: admins{email: "boss@example.com"}}
	_ = b2.Run(t.Context())
	if len(mb.sent) != 1 || ev.count(domain.EventCollaborationBootstrapCompleted) != 1 {
		t.Errorf("repeated: mails %d events %v", len(mb.sent), ev.got)
	}
}

type publishCapture struct {
	evs  *evs
	data *[]map[string]any
}

func (p publishCapture) Publish(ctx context.Context, typ domain.EventType, team, kind, id, agent string, d map[string]any) error {
	*p.data = append(*p.data, d)
	return p.evs.Publish(ctx, typ, team, kind, id, agent, d)
}

func TestBootstrapFailuresAndEdges(t *testing.T) {
	c, srv := client(t)
	mk := func() *boardKeys {
		return &boardKeys{cands: []collab.BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c", Mode: domain.RegistrationManaged, PodReady: true}}, found: true}
	}
	// Mail fails: the invite is withdrawn and the run reports it; the next run invites again.
	mb, ev := &mailbox{err: errors.New("ses down")}, &evs{}
	keys := mk()
	b := &collab.Bootstrap{Keys: keys, Events: ev, Paper: c, Mail: mb, Admins: admins{email: "boss@example.com"}}
	if err := b.Run(t.Context()); err == nil {
		t.Fatal("mail failure")
	}
	if len(srv.Invites()) != 0 || ev.count(domain.EventCollaborationBootstrapCompleted) != 0 {
		t.Errorf("invite kept: %+v %v", srv.Invites(), ev.got)
	}
	mb.err = nil
	if err := b.Run(t.Context()); err != nil || len(mb.sent) != 1 || ev.count(domain.EventCollaborationBootstrapCompleted) != 1 {
		t.Fatalf("retry: %v %+v %v", err, mb.sent, ev.got)
	}
	// Creator is not a person: no mail, the event still follows the record.
	c2, _ := client(t)
	mb2, ev2 := &mailbox{}, &evs{}
	b2 := &collab.Bootstrap{Keys: mk(), Events: ev2, Paper: c2, Mail: mb2, Admins: admins{}}
	if err := b2.Run(t.Context()); err != nil || len(mb2.sent) != 0 || ev2.count(domain.EventCollaborationBootstrapCompleted) != 1 {
		t.Errorf("no person: %v %v", err, ev2.got)
	}
	// Pod not ready: the record is made, the invite and the event wait.
	k3 := mk()
	k3.cands[0].PodReady = false
	ev3 := &evs{}
	b3 := &collab.Bootstrap{Keys: k3, Events: ev3, Paper: c2, Mail: &mailbox{}, Admins: admins{email: "x@example.com"}}
	_ = b3.Run(t.Context())
	if k3.recorded != 1 || len(ev3.got) != 0 {
		t.Errorf("not ready: %d %v", k3.recorded, ev3.got)
	}
	// Admin lookup, invite listing and creation errors surface.
	for _, bb := range []*collab.Bootstrap{
		{Keys: mk(), Events: &evs{}, Paper: c2, Mail: &mailbox{}, Admins: admins{err: errors.New("db")}},
	} {
		if err := bb.Run(t.Context()); err == nil {
			t.Error("admin lookup error")
		}
	}
	srv.FailNext("GET /api/invites", 500)
	if err := (&collab.Bootstrap{Keys: mk(), Events: &evs{}, Paper: c, Mail: &mailbox{}, Admins: admins{email: "a@b.c"}}).Run(t.Context()); err == nil {
		t.Error("list error")
	}
	c3, srv3 := client(t)
	srv3.FailNext("POST /api/invites", 500)
	if err := (&collab.Bootstrap{Keys: mk(), Events: &evs{}, Paper: c3, Mail: &mailbox{}, Admins: admins{email: "a@b.c"}}).Run(t.Context()); err == nil {
		t.Error("create error")
	}
}

type peopleSrc struct{ teams []collab.TeamPeople }

func (p peopleSrc) People(context.Context) ([]collab.TeamPeople, error) { return p.teams, nil }

// COL-33: invites for new people, deactivation for removed ones, admins untouched.
func TestMemberSync(t *testing.T) {
	c, srv := client(t)
	srv.SeedInvite(papertest.Invite{ID: "inv_admin", Email: "boss@example.com", Role: "admin", Accepted: true})
	srv.SeedInvite(papertest.Invite{ID: "inv_old", Email: "gone@example.com", Role: "member", Accepted: true})
	srv.SeedInvite(papertest.Invite{ID: "inv_keep", Email: "Keep@example.com", Role: "member"})
	mb := &mailbox{}
	src := peopleSrc{[]collab.TeamPeople{{TeamID: "team_a", Wanted: []collab.Person{
		{Email: "keep@example.com", Role: collab.RoleMember}, {Email: "new@example.com", Role: collab.RoleViewer}, {Email: "grp@example.com"}, {Email: ""}}}}}
	m := &collab.MemberSync{Source: src, Paper: c, Mail: mb}
	if err := m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(mb.sent) != 2 {
		t.Fatalf("mails: %+v", mb.sent)
	}
	roles := map[string]string{}
	for _, i := range srv.Invites() {
		roles[i.Email] = i.Role
	}
	if roles["new@example.com"] != "viewer" || roles["grp@example.com"] != "member" || roles["boss@example.com"] != "admin" {
		t.Errorf("roles %v", roles)
	}
	if _, ok := roles["gone@example.com"]; ok || !srv.Deactivated("gone@example.com") || srv.Deactivated("boss@example.com") {
		t.Errorf("deactivation: %v", roles)
	}
	// Idempotent.
	n := len(mb.sent)
	_ = m.Run(t.Context())
	if len(mb.sent) != n {
		t.Error("invited again")
	}
	// Mail failure withdraws the invite and reports.
	mb.err = errors.New("ses")
	src.teams[0].Wanted = append(src.teams[0].Wanted, collab.Person{Email: "late@example.com"})
	if err := m.Run(t.Context()); err == nil {
		t.Error("mail error")
	}
	for _, i := range srv.Invites() {
		if i.Email == "late@example.com" {
			t.Error("unsent invite kept")
		}
	}
	srv.FailNext("GET /api/invites", 500)
	if err := m.Run(t.Context()); err == nil {
		t.Error("list error")
	}
}
