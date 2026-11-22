package collab

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type pub struct {
	typ  domain.EventType
	team string
	data map[string]any
}

type fakeEvents struct {
	got []pub
	err error
}

func (f *fakeEvents) Publish(_ context.Context, typ domain.EventType, team, _, _, _ string, data map[string]any) error {
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, pub{typ, team, data})
	return nil
}

func (f *fakeEvents) of(t domain.EventType) (n int) {
	for _, p := range f.got {
		if p.typ == t {
			n++
		}
	}
	return n
}

type fakeStatus struct {
	reports []engine.Report
	err     error
}

func (f *fakeStatus) Apply(_ context.Context, r engine.Report) error {
	if f.err != nil {
		return f.err
	}
	f.reports = append(f.reports, r)
	return nil
}

type fakeSource struct {
	states []TeamState
	err    error
}

func (f fakeSource) States(context.Context) ([]TeamState, error) { return f.states, f.err }

func up() TeamState {
	return TeamState{TeamID: "team_a", AgentID: "agt_c", TeamSlug: "a", TeamState: "active", AgentState: "enabled", PodReady: true,
		BaseDomain: "collab.example.com", PublicEndpoints: true, MayExpose: true, WebUIEnabled: true, ProviderClient: true, Mode: domain.RegistrationManual}
}

func TestComputeEndpointReasons(t *testing.T) {
	tests := []struct {
		name   string
		mod    func(*TeamState)
		ready  bool
		reason string
	}{
		{"ready", func(*TeamState) {}, true, ReasonReady},
		{"suspended", func(s *TeamState) { s.TeamState = "suspended" }, false, ReasonTeamSuspended},
		{"paused", func(s *TeamState) { s.AgentState = "paused" }, false, ReasonPaused},
		{"starting", func(s *TeamState) { s.PodReady = false }, false, ReasonStarting},
		{"service off", func(s *TeamState) { s.PublicEndpoints = false }, false, ReasonServiceOff},
		{"guardrail off", func(s *TeamState) { s.MayExpose = false }, false, ReasonGuardrailOff},
		{"base domain unset", func(s *TeamState) { s.BaseDomain = "" }, false, ReasonBaseDomainUnset},
		{"no surface", func(s *TeamState) { s.WebUIEnabled = false }, false, ReasonRouteNotPublish},
		{"rest only", func(s *TeamState) { s.WebUIEnabled, s.RESTAPIEnabled, s.ProviderClient = false, true, false }, true, ReasonReady},
		{"web without provider client", func(s *TeamState) { s.ProviderClient = false }, false, ReasonRouteNotPublish},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := up()
			tt.mod(&s)
			if e := ComputeEndpoint(s); e.Ready != tt.ready || e.Reason != tt.reason {
				t.Errorf("%+v", e)
			}
		})
	}
	if up().Hostname() != "a.collab.example.com" || (TeamState{TeamSlug: "a"}).Hostname() != "" {
		t.Error("hostname")
	}
}

// RC-24: the status is written every sync; the event only when EndpointReady or its reason changed.
func TestControllerEmitsOnChangeOnly(t *testing.T) {
	ready := true
	notReady := false
	ev, st := &fakeEvents{}, &fakeStatus{}
	s := up()
	s.PrevReady, s.PrevReason = &ready, ReasonReady
	c := &Controller{Source: fakeSource{states: []TeamState{s}}, Status: st, Events: ev, Now: func() time.Time { return t0 }}
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.reports) != 1 || len(ev.got) != 0 || st.reports[0].Phase != domain.PhaseReady {
		t.Fatalf("unchanged: reports=%d events=%d", len(st.reports), len(ev.got))
	}
	s.PodReady = false
	c.Source = fakeSource{states: []TeamState{s}}
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ev.of(domain.EventCollaborationEndpointStateChanged) != 1 {
		t.Fatalf("events: %+v", ev.got)
	}
	d := ev.got[0].data
	if d["ready"] != false || d["previousReady"] != true || d["reason"] != ReasonStarting || d["hostname"] != "a.collab.example.com" {
		t.Errorf("data: %v", d)
	}
	// A brand new endpoint that is starting is not a change; one that becomes ready is.
	s2 := up()
	s2.PodReady = false
	c.Source = fakeSource{states: []TeamState{s2}}
	before := len(ev.got)
	_ = c.Sync(t.Context())
	if len(ev.got) != before {
		t.Error("event for a new starting endpoint")
	}
	s3 := up()
	s3.PrevReady, s3.PrevReason, s3.PaperclipHealthy = &notReady, ReasonStarting, &notReady
	c.Source = fakeSource{states: []TeamState{s3}}
	_ = c.Sync(t.Context())
	if len(ev.got) != before+1 || len(st.reports[len(st.reports)-1].Conditions) != 2 {
		t.Errorf("ready transition: %d events", len(ev.got)-before)
	}
}

func TestControllerErrors(t *testing.T) {
	c := &Controller{Source: fakeSource{err: errors.New("db")}, Status: &fakeStatus{}, Events: &fakeEvents{}, Now: func() time.Time { return t0 }}
	if err := c.Sync(t.Context()); err == nil {
		t.Error("source error")
	}
	c.Source = fakeSource{states: []TeamState{up()}}
	c.Status = &fakeStatus{err: errors.New("write")}
	if err := c.Sync(t.Context()); err == nil || !strings.Contains(err.Error(), "1 of 1") {
		t.Errorf("status error: %v", err)
	}
}

type fakeLinks struct {
	links []Link
	set   []domain.LinkState
}

func (f *fakeLinks) Links(context.Context) ([]Link, error) { return f.links, nil }
func (f *fakeLinks) SetState(_ context.Context, _, _ string, st domain.LinkState, _ string, _ time.Time) error {
	f.set = append(f.set, st)
	return nil
}

// RC-22: a connected worker with an id and a key value is linked; leaving linked is announced.
func TestLinkSync(t *testing.T) {
	l := Link{TeamID: "team_a", AgentID: "agt_w", Connect: true, LinkState: domain.LinkPending, PaperclipAgentID: "pc-1", KeyPresent: true}
	src, ev := &fakeLinks{links: []Link{l}}, &fakeEvents{}
	s := &LinkSync{Source: src, Events: ev, Now: func() time.Time { return t0 }}
	if err := s.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(src.set) != 1 || src.set[0] != domain.LinkLinked || ev.of(domain.EventAgentCollaborationLinked) != 1 {
		t.Fatalf("link: %v %+v", src.set, ev.got)
	}
	// Steady state: no write, no event.
	src.links[0].LinkState = domain.LinkLinked
	_ = s.Sync(t.Context())
	if len(src.set) != 1 || len(ev.got) != 1 {
		t.Fatal("steady state wrote")
	}
	// Disconnect: the database resets the state to disabled; the unlink is announced with its reason.
	src.links[0].Connect, src.links[0].LinkState = false, domain.LinkDisabled
	_ = s.Sync(t.Context())
	if ev.of(domain.EventAgentCollaborationUnlinked) != 1 || ev.got[1].data["reason"] != "disconnected" {
		t.Fatalf("unlink: %+v", ev.got)
	}
	// A key without a value stays pending; a managed team's rows are not touched here.
	src.links = []Link{{TeamID: "team_a", AgentID: "agt_x", Connect: true, LinkState: domain.LinkPending, PaperclipAgentID: "pc"},
		{TeamID: "team_a", AgentID: "agt_m", Connect: true, LinkState: domain.LinkPending, Mode: domain.RegistrationManaged}}
	n := len(src.set)
	_ = s.Sync(t.Context())
	if len(src.set) != n {
		t.Error("pending or managed rows written")
	}
	for _, c := range []struct {
		l    Link
		want domain.LinkState
		why  string
	}{
		{Link{Connect: true, AgentGone: true}, domain.LinkDisabled, "agent_deleted"},
		{Link{Connect: true, CollabAgentGone: true}, domain.LinkDisabled, "collaboration_agent_deleted"},
	} {
		if st, why := Observed(c.l); st != c.want || why != c.why {
			t.Errorf("%+v: %s %s", c.l, st, why)
		}
	}
	// A process that starts with a linked row and sees it gone announces the unlink once.
	src.links = []Link{{TeamID: "team_a", AgentID: "agt_y", Connect: false, LinkState: domain.LinkLinked}}
	fresh := &LinkSync{Source: src, Events: ev, Now: func() time.Time { return t0 }}
	_ = fresh.Sync(t.Context())
	if ev.of(domain.EventAgentCollaborationUnlinked) != 2 {
		t.Error("fresh process missed the unlink")
	}
}

type fakeAccess struct {
	rows []Denied
	thr  int
	last time.Time
}

func (f fakeAccess) DeniedSince(context.Context, time.Time) ([]Denied, error) { return f.rows, nil }
func (f fakeAccess) Threshold(context.Context) (int, error)                   { return f.thr, nil }
func (f fakeAccess) LastSpike(context.Context, string) (time.Time, error)     { return f.last, nil }

// RC-27: strictly more than the threshold, once per team per 15 minutes; a failover rebuilds the cooldown.
func TestSpikeDetector(t *testing.T) {
	clock := t0
	ev := &fakeEvents{}
	d := &SpikeDetector{Source: fakeAccess{thr: 50, rows: []Denied{{TeamID: "team_a", Unauthenticated: 30, Forbidden: 21}, {TeamID: "team_b", Forbidden: 50}}},
		Events: ev, Now: func() time.Time { return clock }}
	if err := d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ev.got) != 1 || ev.got[0].typ != domain.EventCollaborationDeniedSpike || ev.got[0].team != "team_a" || ev.got[0].data["denied"] != 51 {
		t.Fatalf("events: %+v", ev.got)
	}
	clock = clock.Add(14 * time.Minute)
	_ = d.Run(t.Context())
	if len(ev.got) != 1 {
		t.Error("inside the cooldown")
	}
	clock = clock.Add(2 * time.Minute)
	_ = d.Run(t.Context())
	if len(ev.got) != 2 {
		t.Error("after the cooldown")
	}
	// New leader: the cooldown comes from the last event in the outbox.
	ev2 := &fakeEvents{}
	d2 := &SpikeDetector{Source: fakeAccess{thr: 50, rows: []Denied{{TeamID: "team_a", Forbidden: 99}}, last: clock.Add(-time.Minute)},
		Events: ev2, Now: func() time.Time { return clock }}
	_ = d2.Run(t.Context())
	if len(ev2.got) != 0 {
		t.Error("failover repeated a recent event")
	}
	if BucketFloor(t0.Add(3*time.Minute)).Unix()%300 != 0 {
		t.Error("BucketFloor")
	}
}

type fakePaper struct {
	roster []RosterAgent
	items  []Item
}

func (f fakePaper) Roster(context.Context, string) ([]RosterAgent, error)      { return f.roster, nil }
func (f fakePaper) Changed(context.Context, string, time.Time) ([]Item, error) { return f.items, nil }

// RC-26: unmanaged agents are announced once per new name and clear when gone; secrets in content are
// reported by pattern and counted, never copied.
func TestGuard(t *testing.T) {
	s := up()
	s.Mode = domain.RegistrationManaged
	ev, st := &fakeEvents{}, &fakeStatus{}
	p := fakePaper{roster: []RosterAgent{{ID: "1", Name: "ceo", Orchestrator: true}, {ID: "2", Name: "rogue<script>"}, {ID: "3", Name: "local", LocalAdapter: true},
		{ID: "4", Name: "ok", LinkedAFSAgentID: "agt_w"}},
		items: []Item{{ID: "iss_1", AuthorAgentID: "4", UpdatedAt: t0, Text: "key AKIAIOSFODNN7EXAMPLE"}, {ID: "iss_2", Text: "nothing"}}} //nolint:gosec // fake credential to prove the scanner
	g := &Guard{Source: fakeSource{states: []TeamState{s, up()}}, Paper: p, Status: st, Events: ev, Now: func() time.Time { return t0 }}
	if err := g.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ev.of(domain.EventCollaborationUnmanagedAgentsFound) != 2 || ev.of(domain.EventCollaborationSecretDetected) != 1 {
		t.Fatalf("events: %+v", ev.got)
	}
	c := st.reports[0].Conditions[0]
	if c.Type != ConditionUnmanaged || c.Status != domain.ConditionTrue || strings.Contains(c.Message, "<") {
		t.Errorf("condition: %+v", c)
	}
	for _, e := range ev.got {
		if e.typ == domain.EventCollaborationSecretDetected {
			if e.data["pattern"] != "aws_access_key" || e.data["action"] != "detected" || e.data["source"] != "reconciler" {
				t.Errorf("secret event: %v", e.data)
			}
			for _, v := range e.data {
				if str, ok := v.(string); ok && strings.Contains(str, "AKIA") {
					t.Error("event carries the matched text")
				}
			}
		}
	}
	// Second run: the same names are not announced again, the same items are not rescanned by cursor.
	n := len(ev.got)
	_ = g.Run(t.Context())
	if ev.of(domain.EventCollaborationUnmanagedAgentsFound) != 2 {
		t.Error("announced again")
	}
	if ev.of(domain.EventCollaborationSecretDetected) != 1 {
		t.Error("the same item version was reported twice")
	}
	_ = n
	// The names clear when gone.
	g.Paper = fakePaper{roster: []RosterAgent{{ID: "1", Name: "ceo", Orchestrator: true}}}
	_ = g.Run(t.Context())
	if last := st.reports[len(st.reports)-1].Conditions[0]; last.Status != domain.ConditionFalse {
		t.Errorf("not cleared: %+v", last)
	}
	// After a leader change the stored names seed the announced set.
	s.Unmanaged = []string{"rogue<script>", "local"}
	ev3 := &fakeEvents{}
	g3 := &Guard{Source: fakeSource{states: []TeamState{s}}, Paper: p, Status: &fakeStatus{}, Events: ev3, Now: func() time.Time { return t0 }}
	_ = g3.Run(t.Context())
	if ev3.of(domain.EventCollaborationUnmanagedAgentsFound) != 0 {
		t.Error("re-announced after failover")
	}
	if got := SanitizeNames([]string{strings.Repeat("x", 600)}); len(got) != 500 {
		t.Errorf("cut: %d", len(got))
	}
}

type fakeKeys struct {
	pending  []BoardKeyCandidate
	found    bool
	recorded []string
	err      error
}

func (f *fakeKeys) Pending(context.Context) ([]BoardKeyCandidate, error) { return f.pending, nil }
func (f *fakeKeys) Find(context.Context, string, string) (string, bool, error) {
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/ns/agt/paperclip-board-api-key-AbCdEf", f.found, f.err
}
func (f *fakeKeys) Record(_ context.Context, team, agent, _ string) error {
	f.recorded = append(f.recorded, team+"/"+agent)
	return nil
}

// RC-29: the event is sent once the board key is recorded, and never carries the key or an invite.
func TestBootstrap(t *testing.T) {
	k, ev := &fakeKeys{pending: []BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c", Mode: domain.RegistrationManaged}}}, &fakeEvents{}
	b := &Bootstrap{Keys: k, Events: ev}
	if err := b.Run(t.Context()); err != nil || len(k.recorded) != 0 || len(ev.got) != 0 {
		t.Fatalf("no key yet: %v %v %v", err, k.recorded, ev.got)
	}
	k.found = true
	if err := b.Run(t.Context()); err != nil || len(k.recorded) != 1 || ev.of(domain.EventCollaborationBootstrapCompleted) != 1 {
		t.Fatalf("recorded: %v %v", k.recorded, ev.got)
	}
	if d := ev.got[0].data; d["mode"] != "managed" || len(d) != 3 {
		t.Errorf("data: %v", d)
	}
	k.err = errors.New("sm")
	if err := b.Run(t.Context()); err == nil {
		t.Error("find error")
	}
}

type fakeHK struct {
	months []string
	purged int64
	err    error
}

func (f *fakeHK) EnsureAccessPartition(_ context.Context, m string) error {
	f.months = append(f.months, m)
	return f.err
}
func (f *fakeHK) PurgeEdgeSessions(context.Context) (int64, error) { return f.purged, f.err }

func TestHousekeeping(t *testing.T) {
	hk := &fakeHK{purged: 4}
	if err := EnsurePartitions(t.Context(), hk, func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if len(hk.months) != 3 || hk.months[0] != "2026-11-01" || hk.months[2] != "2027-01-01" {
		t.Errorf("months %v", hk.months)
	}
	if err := PurgeSessions(t.Context(), hk, nil); err != nil {
		t.Fatal(err)
	}
	hk.err = errors.New("x")
	if EnsurePartitions(t.Context(), hk, time.Now) == nil || PurgeSessions(t.Context(), hk, nil) == nil {
		t.Error("errors")
	}
}

type errPaper struct{ fakePaper }

func (errPaper) Roster(context.Context, string) ([]RosterAgent, error) {
	return nil, errors.New("paperclip down")
}

type errLinks struct{ fakeLinks }

func (errLinks) SetState(context.Context, string, string, domain.LinkState, string, time.Time) error {
	return errors.New("db down")
}

type failKeys struct {
	fakeKeys
	failRecord bool
}

func (f *failKeys) Record(context.Context, string, string, string) error {
	if f.failRecord {
		return errors.New("db down")
	}
	return nil
}

// A failure of one team or row is logged, counted and returned; the others still run.
func TestFailuresAreCountedAndReturned(t *testing.T) {
	managed := TeamState{TeamID: "team_a", Mode: domain.RegistrationManaged, PodReady: true}
	g := &Guard{Source: fakeSource{states: []TeamState{managed}}, Paper: errPaper{}, Status: &fakeStatus{}, Events: &fakeEvents{}, Now: func() time.Time { return t0 }}
	if err := g.Run(t.Context()); err == nil {
		t.Error("guard: roster failure swallowed")
	}
	g = &Guard{Source: fakeSource{err: errors.New("x")}, Now: func() time.Time { return t0 }}
	if err := g.Run(t.Context()); err == nil {
		t.Error("guard: source failure swallowed")
	}
	ls := &LinkSync{Source: &errLinks{fakeLinks{links: []Link{{TeamID: "team_a", AgentID: "agt_w", Connect: true, LinkState: domain.LinkPending, PaperclipAgentID: "p", KeyPresent: true}}}},
		Events: &fakeEvents{}, Now: func() time.Time { return t0 }}
	if err := ls.Sync(t.Context()); err == nil {
		t.Error("links: set-state failure swallowed")
	}
	ev := &fakeEvents{err: errors.New("outbox")}
	b := &Bootstrap{Keys: &failKeys{fakeKeys: fakeKeys{pending: []BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c"}}, found: true}}, Events: ev}
	if err := b.Run(t.Context()); err == nil {
		t.Error("bootstrap: event failure swallowed")
	}
	b = &Bootstrap{Keys: &failKeys{fakeKeys: fakeKeys{pending: []BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c"}}, found: true}, failRecord: true}, Events: &fakeEvents{}}
	if err := b.Run(t.Context()); err == nil {
		t.Error("bootstrap: record failure swallowed")
	}
	b = &Bootstrap{Keys: &fakeKeys{pending: []BoardKeyCandidate{{TeamID: "team_a", AgentID: "agt_c"}}, err: errors.New("sm")}, Events: &fakeEvents{}}
	if err := b.Run(t.Context()); err == nil {
		t.Error("bootstrap: find failure swallowed")
	}
	if boolReason(true, "a", "b") != "a" || boolReason(false, "a", "b") != "b" || modeOr("") != domain.RegistrationManual || modeOr(domain.RegistrationManaged) != domain.RegistrationManaged {
		t.Error("helpers")
	}
	if phaseOf(TeamState{}) != domain.PhasePending {
		t.Error("phaseOf")
	}
	if (&Controller{}).log() == nil || (&Guard{}).log() == nil || (&LinkSync{}).log() == nil {
		t.Error("default logger")
	}
}

type fakeCIDRs struct {
	list []string
	err  error
}

func (f fakeCIDRs) AllowedCIDRs(context.Context) ([]string, error) { return f.list, f.err }

type fakeSets struct {
	have    map[string][]string
	updated map[string][]string
	err     error
}

func (f *fakeSets) Addresses(_ context.Context, r IPSetRef) ([]string, error) {
	return append([]string(nil), f.have[r.ARN]...), f.err
}

func (f *fakeSets) Update(_ context.Context, r IPSetRef, a []string) error {
	if f.updated == nil {
		f.updated = map[string][]string{}
	}
	f.updated[r.ARN] = a
	return f.err
}

// RC-28: the IP sets follow collab.allowed_source_cidrs; an empty list is the whole internet.
func TestWAFSync(t *testing.T) {
	v4, v6 := IPSetRef{ARN: "v4"}, IPSetRef{ARN: "v6", IPv6: true}
	run := func(cidrs []string, have map[string][]string) (*fakeSets, error) {
		s := &fakeSets{have: have}
		return s, (&WAFSync{CIDRs: fakeCIDRs{list: cidrs}, Sets: s, V4: v4, V6: v6}).Run(t.Context())
	}
	s, err := run(nil, map[string][]string{})
	if err != nil || len(s.updated["v4"]) != 1 || s.updated["v4"][0] != "0.0.0.0/0" || s.updated["v6"][0] != "::/0" {
		t.Fatalf("empty list: %v %v", s.updated, err)
	}
	s, err = run([]string{"203.0.113.9/24", "2001:db8::/32", "203.0.113.0/24"}, map[string][]string{"v4": {"203.0.113.0/24"}, "v6": {"::/0"}})
	if err != nil || s.updated["v4"] != nil || len(s.updated["v6"]) != 1 || s.updated["v6"][0] != "2001:db8::/32" {
		t.Fatalf("masked and compacted: %v %v", s.updated, err)
	}
	s, err = run([]string{"198.51.100.0/24"}, map[string][]string{"v4": {"0.0.0.0/0"}, "v6": {"::/0"}})
	if _, written := s.updated["v6"]; err != nil || !written || len(s.updated["v6"]) != 0 {
		t.Errorf("a list of only IPv4 must close IPv6: %v %v", s.updated, err)
	}
	if _, err := run([]string{"not-a-cidr"}, nil); err == nil {
		t.Error("bad CIDR accepted")
	}
	if err := (&WAFSync{CIDRs: fakeCIDRs{err: errors.New("db")}, Sets: &fakeSets{}}).Run(t.Context()); err == nil {
		t.Error("settings failure swallowed")
	}
	if err := (&WAFSync{CIDRs: fakeCIDRs{}, Sets: &fakeSets{err: errors.New("waf")}, V4: v4}).Run(t.Context()); err == nil {
		t.Error("WAF failure swallowed")
	}
	// An unset IP set ARN is skipped.
	if s, err := run(nil, nil); err != nil || s == nil {
		t.Fatal(err)
	}
	if err := (&WAFSync{CIDRs: fakeCIDRs{}, Sets: &fakeSets{err: errors.New("x")}}).Run(t.Context()); err != nil {
		t.Errorf("no sets configured: %v", err)
	}
}
