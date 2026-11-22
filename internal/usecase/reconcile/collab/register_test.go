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
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var now0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type target paperclip.Target

func (t target) Target(context.Context, string) (paperclip.Target, error) {
	return paperclip.Target(t), nil
}

type evs struct{ got []domain.EventType }

func (e *evs) Publish(_ context.Context, typ domain.EventType, _, _, _, _ string, _ map[string]any) error {
	e.got = append(e.got, typ)
	return nil
}

func (e *evs) count(t domain.EventType) (n int) {
	for _, g := range e.got {
		if g == t {
			n++
		}
	}
	return n
}

type stat struct{ reports []engine.Report }

func (s *stat) Apply(_ context.Context, r engine.Report) error {
	s.reports = append(s.reports, r)
	return nil
}
func (s *stat) last() domain.Condition {
	r := s.reports[len(s.reports)-1]
	return r.Conditions[len(r.Conditions)-1]
}

// memSource is the database: the links, the recorded keys and the writes.
type memSource struct {
	teams     []collab.ManagedTeam
	recordErr error
	removeErr error
	removed   []string
	recorded  map[string]string
}

func (m *memSource) ManagedTeams(context.Context) ([]collab.ManagedTeam, error) {
	// Return copies so the registrar sees what the database holds now.
	out := make([]collab.ManagedTeam, len(m.teams))
	for i, t := range m.teams {
		t.Links = append([]collab.ManagedLink(nil), t.Links...)
		out[i] = t
	}
	return out, nil
}

func (m *memSource) link(team, agent string) *collab.ManagedLink {
	for i := range m.teams {
		for j := range m.teams[i].Links {
			if l := &m.teams[i].Links[j]; l.TeamID == team && l.AgentID == agent {
				return l
			}
		}
	}
	return nil
}

func (m *memSource) SetManaged(_ context.Context, team, agent string, u collab.ManagedUpdate) error {
	l := m.link(team, agent)
	l.LinkState, l.PaperclipAgentID = u.State, u.PaperclipAgentID
	l.LastError = u.LastError
	return nil
}

func (m *memSource) RecordKey(_ context.Context, team, agent, arn string) error {
	if m.recordErr != nil {
		return m.recordErr
	}
	if m.recorded == nil {
		m.recorded = map[string]string{}
	}
	m.recorded[agent] = arn
	m.link(team, agent).KeyARN = arn
	return nil
}

func (m *memSource) RemoveKey(_ context.Context, team, agent string) (bool, error) {
	if m.removeErr != nil {
		return false, m.removeErr
	}
	m.removed = append(m.removed, agent)
	m.link(team, agent).KeyARN = ""
	delete(m.recorded, agent)
	return true, nil
}

// memKeys is Secrets Manager.
type memKeys struct {
	values  map[string]string
	deleted []string
	putErr  error
}

func (k *memKeys) Put(_ context.Context, path string, v collab.Key, tags map[string]string) (string, error) {
	if k.putErr != nil {
		return "", k.putErr
	}
	if k.values == nil {
		k.values = map[string]string{}
	}
	if tags["afs-team-id"] == "" || tags["afs-agent-id"] == "" {
		return "", errors.New("tags")
	}
	k.values[path] = v.Reveal()
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + path + "-AbCdEf", nil
}

func (k *memKeys) ScheduleDeletion(_ context.Context, arn string) error {
	k.deleted = append(k.deleted, arn)
	return nil
}

type rig struct {
	reg  *collab.Registrar
	src  *memSource
	keys *memKeys
	srv  *papertest.Server
	ev   *evs
	st   *stat
	logs *bytes.Buffer
}

func newRig(t *testing.T, links ...collab.ManagedLink) *rig {
	t.Helper()
	srv := papertest.New("board-secret")
	t.Cleanup(srv.Close)
	for i := range links {
		links[i].TeamID = "team_a"
		if links[i].KeyPath == "" {
			links[i].KeyPath = "afs/ns/" + links[i].AgentID + "/paperclip-agent-api-key"
		}
		if links[i].LinkState == "" {
			links[i].LinkState = domain.LinkPending
		}
	}
	src := &memSource{teams: []collab.ManagedTeam{{TeamID: "team_a", CollabAgentID: "agt_c", PodReady: true, Links: links}}}
	r := &rig{src: src, keys: &memKeys{}, srv: srv, ev: &evs{}, st: &stat{}, logs: &bytes.Buffer{}}
	r.reg = &collab.Registrar{Source: src, Paper: paperclip.New(target{BaseURL: srv.URL, BoardKey: "board-secret"}), Keys: r.keys, Status: r.st, Events: r.ev,
		Now: func() time.Time { return now0 }, Log: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return r
}

func worker(id string) collab.ManagedLink {
	return collab.ManagedLink{AgentID: id, AgentName: "w-" + id, Persona: "helper", Connect: true}
}

// RC-25 / COL-12: a connected agent gets a Paperclip agent, its key goes to Secrets Manager and the
// records are written; the key reaches no log, event or condition.
func TestRegistersAConnectedAgentAndNeverLogsTheKey(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := r.src.link("team_a", "agt_1")
	if l.LinkState != domain.LinkLinked || l.PaperclipAgentID == "" || l.KeyARN == "" {
		t.Fatalf("link: %+v", l)
	}
	pa, _ := r.srv.AgentByID(l.PaperclipAgentID)
	if pa.Name != "w-agt_1" || pa.AFSAgentID != "agt_1" || pa.AdapterType != "remote" {
		t.Errorf("paperclip agent: %+v", pa)
	}
	if r.keys.values["afs/ns/agt_1/paperclip-agent-api-key"] != pa.Key || pa.Key == "" {
		t.Error("the key did not reach Secrets Manager")
	}
	if r.ev.count(domain.EventAgentCollaborationLinked) != 1 {
		t.Errorf("events %v", r.ev.got)
	}
	if c := r.st.last(); c.Type != collab.ConditionRegistrationHealthy || c.Status != domain.ConditionTrue {
		t.Errorf("condition %+v", c)
	}
	if strings.Contains(r.logs.String(), pa.Key) || strings.Contains(r.logs.String(), "board-secret") {
		t.Error("a key reached the log")
	}
	// A second pass changes nothing.
	calls := len(r.srv.Calls())
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.ev.count(domain.EventAgentCollaborationLinked) != 1 {
		t.Error("linked twice")
	}
	if n := len(r.srv.Calls()) - calls; n != 1 { // only the roster read
		t.Errorf("second pass made %d calls", n)
	}
}

func TestKeyNeverInLogsEvenOnFailure(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	r.src.recordErr = errors.New("db down")
	_ = r.reg.Run(t.Context())
	pa, _ := r.srv.AgentByName("w-agt_1")
	if strings.Contains(r.logs.String(), "pck_secret") || strings.Contains(r.logs.String(), pa.Key) && pa.Key != "" {
		t.Errorf("log: %s", r.logs.String())
	}
	if pa.Status != "revoked" {
		t.Errorf("the agent whose key could not be recorded must be revoked: %+v", pa)
	}
	l := r.src.link("team_a", "agt_1")
	if l.LinkState != domain.LinkFailed || l.PaperclipAgentID != "" || l.LastError == "" {
		t.Errorf("link: %+v", l)
	}
	if len(r.keys.deleted) != 1 {
		t.Error("the stored secret was not scheduled for deletion")
	}
	if c := r.st.last(); c.Status != domain.ConditionFalse || c.Reason != "RegistrationFailed" || !strings.Contains(c.Message, "agt_1") {
		t.Errorf("condition %+v", c)
	}
	// It recovers on the next pass.
	r.src.recordErr = nil
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.src.link("team_a", "agt_1").LinkState != domain.LinkLinked {
		t.Error("did not recover")
	}
}

func TestCreateFailureIsFailed(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	r.srv.FailNext("POST /api/agents", 500)
	if err := r.reg.Run(t.Context()); err == nil {
		t.Fatal("want an error")
	}
	if l := r.src.link("team_a", "agt_1"); l.LinkState != domain.LinkFailed || !strings.Contains(l.LastError, "500") {
		t.Errorf("%+v", l)
	}
	r.keys.putErr = errors.New("sm")
	_ = r.reg.Run(t.Context())
	if pa, _ := r.srv.AgentByName("w-agt_1"); pa.Status != "revoked" {
		t.Errorf("put failure must revoke: %+v", pa)
	}
}

// Pausing an AFS agent pauses its Paperclip agent, and resuming resumes it.
func TestPauseFollowsTheAgent(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	_ = r.reg.Run(t.Context())
	l := r.src.link("team_a", "agt_1")
	l.Paused = true
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pa, _ := r.srv.AgentByID(l.PaperclipAgentID); pa.Status != "paused" {
		t.Errorf("not paused: %+v", pa)
	}
	l.Paused = false
	_ = r.reg.Run(t.Context())
	if pa, _ := r.srv.AgentByID(l.PaperclipAgentID); pa.Status != "active" {
		t.Errorf("not resumed: %+v", pa)
	}
	// An agent that is paused when first connected is created paused.
	r2 := newRig(t, func() collab.ManagedLink { w := worker("agt_2"); w.Paused = true; return w }())
	_ = r2.reg.Run(t.Context())
	if pa, _ := r2.srv.AgentByName("w-agt_2"); pa.Status != "paused" {
		t.Errorf("created active: %+v", pa)
	}
	r2.srv.FailNext("POST /api/agents/", 500)
	r3 := newRig(t, worker("agt_3"))
	_ = r3.reg.Run(t.Context())
	r3.src.link("team_a", "agt_3").Paused = true
	r3.srv.FailNext("POST /api/agents/", 500)
	if err := r3.reg.Run(t.Context()); err == nil {
		t.Error("pause failure")
	}
}

// Disconnecting or deleting revokes the key, removes the records and schedules the secret for deletion.
func TestDisconnectAndDeleteRevoke(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*collab.ManagedLink)
		reason string
	}{
		{"disconnect", func(l *collab.ManagedLink) { l.Connect = false }, "disconnected"},
		{"agent deleted", func(l *collab.ManagedLink) { l.AgentGone = true }, "agent_deleted"},
		{"collaboration agent deleted", func(l *collab.ManagedLink) { l.CollabGone = true }, "collaboration_agent_deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, worker("agt_1"))
			_ = r.reg.Run(t.Context())
			l := r.src.link("team_a", "agt_1")
			id, arn := l.PaperclipAgentID, l.KeyARN
			tc.mutate(l)
			if err := r.reg.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			// With the collaboration agent gone Paperclip is gone too: there is nothing to revoke.
			if pa, _ := r.srv.AgentByID(id); tc.reason != "collaboration_agent_deleted" && (pa.Status != "revoked" || pa.Key != "") {
				t.Errorf("not revoked: %+v", pa)
			}
			if len(r.src.removed) != 1 || len(r.keys.deleted) != 1 || r.keys.deleted[0] != arn {
				t.Errorf("records: removed %v deleted %v", r.src.removed, r.keys.deleted)
			}
			if l.LinkState != domain.LinkDisabled || l.PaperclipAgentID != "" {
				t.Errorf("link %+v", l)
			}
			if r.ev.count(domain.EventAgentCollaborationUnlinked) != 1 {
				t.Errorf("events %v", r.ev.got)
			}
			// Nothing more happens.
			n := len(r.srv.Calls())
			_ = r.reg.Run(t.Context())
			if len(r.srv.Calls())-n > 1 || len(r.keys.deleted) != 1 {
				t.Error("teardown repeated")
			}
		})
	}
}

func TestTeardownFailuresKeepTheIDForRetry(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	_ = r.reg.Run(t.Context())
	l := r.src.link("team_a", "agt_1")
	id := l.PaperclipAgentID
	l.Connect = false
	r.srv.FailNext("POST /api/agents/", 500)
	if err := r.reg.Run(t.Context()); err == nil {
		t.Fatal("revoke failure")
	}
	if l.PaperclipAgentID != id || l.LastError == "" {
		t.Errorf("id lost: %+v", l)
	}
	r.src.removeErr = errors.New("db")
	if err := r.reg.Run(t.Context()); err == nil {
		t.Fatal("remove failure")
	}
	r.src.removeErr = nil
	if err := r.reg.Run(t.Context()); err != nil || l.LinkState != domain.LinkDisabled {
		t.Fatalf("retry: %v %+v", err, l)
	}
}

// A connected agent whose Paperclip agent was removed by hand is registered again.
func TestDriftReRegisters(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	_ = r.reg.Run(t.Context())
	l := r.src.link("team_a", "agt_1")
	old := l.PaperclipAgentID
	r.srv.RemoveAgent(old)
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.PaperclipAgentID == "" || l.PaperclipAgentID == old || l.LinkState != domain.LinkLinked {
		t.Errorf("link %+v", l)
	}
	if len(r.keys.deleted) != 1 {
		t.Errorf("old secret not scheduled: %v", r.keys.deleted)
	}
}

// An id without a key (a registration that stopped half way) is cleaned and redone.
func TestHalfRegisteredIsRedone(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	r.srv.AddAgent(papertest.Agent{ID: "pc_half", Name: "w-agt_1", AdapterType: "remote", AFSAgentID: "agt_1"})
	l := r.src.link("team_a", "agt_1")
	l.PaperclipAgentID = "pc_half"
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pa, _ := r.srv.AgentByID("pc_half"); pa.Status != "revoked" {
		t.Errorf("half agent kept: %+v", pa)
	}
	if l.LinkState != domain.LinkLinked || l.PaperclipAgentID == "pc_half" {
		t.Errorf("%+v", l)
	}
}

func TestTeamsThatAreNotReadyAreSkipped(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	r.src.teams[0].PodReady = false
	if err := r.reg.Run(t.Context()); err != nil || len(r.srv.Calls()) != 0 {
		t.Fatalf("%v %v", err, r.srv.Calls())
	}
}

func TestPaperclipDownSetsRegistrationUnhealthy(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	r.srv.FailNext("GET /api/agents", 503)
	if err := r.reg.Run(t.Context()); err == nil {
		t.Fatal("want an error")
	}
	if c := r.st.last(); c.Status != domain.ConditionFalse || c.Reason != "PaperclipUnreachable" {
		t.Errorf("%+v", c)
	}
	if r.src.link("team_a", "agt_1").LinkState != domain.LinkPending {
		t.Error("link touched while Paperclip is down")
	}
}

func TestNoLinksIsHealthy(t *testing.T) {
	r := newRig(t)
	if err := r.reg.Run(t.Context()); err != nil || r.st.last().Status != domain.ConditionTrue {
		t.Fatal("no links")
	}
}

func TestKeyType(t *testing.T) {
	k := collab.Key("sekret")
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("k", "key", k, "m", map[string]any{"k": k})
	if strings.Contains(buf.String(), "sekret") {
		t.Errorf("log: %s", buf.String())
	}
	if b, _ := k.MarshalJSON(); strings.Contains(string(b), "sekret") || k.String() != "[redacted]" {
		t.Error("marshal")
	}
}

// A team that lost its collaboration agent: Paperclip went with the pod, so nothing is called; the records
// are removed and the secret is scheduled for deletion.
func TestGoneTeamIsCleanedWithoutCallingPaperclip(t *testing.T) {
	r := newRig(t, worker("agt_1"))
	_ = r.reg.Run(t.Context())
	l := r.src.link("team_a", "agt_1")
	l.LinkState = domain.LinkLinked
	r.src.teams[0].Gone, r.src.teams[0].CollabAgentID = true, ""
	before := len(r.srv.Calls())
	if err := r.reg.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.srv.Calls()) != before || len(r.keys.deleted) != 1 || l.LinkState != domain.LinkDisabled {
		t.Errorf("calls %d keys %v link %+v", len(r.srv.Calls())-before, r.keys.deleted, l)
	}
	r.src.removeErr = errors.New("db")
	l.PaperclipAgentID, l.KeyARN, l.LinkState = "pc_x", "arn:x", domain.LinkLinked
	if err := r.reg.Run(t.Context()); err == nil {
		t.Error("cleanup error")
	}
}
