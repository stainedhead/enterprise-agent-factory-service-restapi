package traffic_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

const (
	team = "team_01JD0000000000000000000051"
	agtA = "agt_01JD0000000000000000000061"
	pool = "pool_01JD0000000000000000000091"
	ns   = "afs-t-tr"
)

var t0 = time.Date(2026, 11, 22, 10, 2, 30, 0, time.UTC)

func sys(id, slug, cat, off string, eps ...string) egress.System {
	return egress.System{ID: id, Slug: slug, Category: cat, Endpoints: eps, DisabledAt: off}
}

type fakeDir struct {
	mu      sync.Mutex
	systems []egress.System
	// known maps "ns/id" to existence.
	known map[string]bool
	calls int
	err   error
}

func (d *fakeDir) resolve(ns, id string) (traffic.Resolved, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return traffic.Resolved{}, false, d.err
	}
	if !d.known[ns+"/"+id] {
		return traffic.Resolved{}, false, nil
	}
	return traffic.Resolved{TeamID: team, Systems: d.systems}, true, nil
}
func (d *fakeDir) ResolveAgent(_ context.Context, ns, id string) (traffic.Resolved, bool, error) {
	return d.resolve(ns, id)
}
func (d *fakeDir) ResolvePool(_ context.Context, ns, id string) (traffic.Resolved, bool, error) {
	return d.resolve(ns, id)
}

type fakeStore struct {
	mu   sync.Mutex
	rows []traffic.Row
	fail error
	adds int
}

func (s *fakeStore) Add(_ context.Context, rows []traffic.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds++
	if s.fail != nil {
		return s.fail
	}
	s.rows = append(s.rows, rows...)
	return nil
}

type maskRedactor struct{}

// Redact stands in for usecase/redact: anything that looks like an AWS key id is replaced.
func (maskRedactor) Redact(s string) string {
	if strings.Contains(s, "akiaiosfodnn7example") {
		return strings.ReplaceAll(s, "akiaiosfodnn7example", "[redacted:abcd1234]")
	}
	return s
}

type recordedSignIns struct {
	got []traffic.SignIn
	err error
}

func (r *recordedSignIns) Record(_ context.Context, s traffic.SignIn) error {
	r.got = append(r.got, s)
	return r.err
}

type recordedObserver struct{ batches [][]traffic.Row }

func (o *recordedObserver) Observe(_ context.Context, rows []traffic.Row) {
	o.batches = append(o.batches, rows)
}

func agentLabels() []string {
	return []string{"k8s:" + traffic.LabelAgentID + "=" + agtA, "k8s:app=agent"}
}
func poolLabels() []string { return []string{"k8s:" + traffic.LabelPoolID + "=" + pool} }

func flow(mod func(*traffic.Flow)) traffic.Flow {
	f := traffic.Flow{Time: t0, Egress: true, Forwarded: true, Proto: "tcp", SYN: true, DstPort: 443,
		DstNames: []string{"api.github.com"}, SrcNamespace: ns, SrcPod: "agent-alpha-0", SrcLabels: agentLabels()}
	if mod != nil {
		mod(&f)
	}
	return f
}

func denied(mod func(*traffic.Flow)) traffic.Flow {
	return flow(func(f *traffic.Flow) {
		f.Forwarded, f.PolicyDenied = false, true
		if mod != nil {
			mod(f)
		}
	})
}

func newCollector(dir *fakeDir, st *fakeStore, extra func(*traffic.Config)) *traffic.Collector {
	cfg := traffic.Config{Directory: dir, Store: st, Redactor: maskRedactor{}, Now: func() time.Time { return t0 }}
	if extra != nil {
		extra(&cfg)
	}
	return traffic.New(cfg)
}

func world() *fakeDir {
	return &fakeDir{
		known: map[string]bool{ns + "/" + agtA: true, ns + "/" + pool: true},
		systems: []egress.System{
			sys("ext_gh", "github", "dev_tools", "", "api.github.com", "*.githubusercontent.com"),
			sys("ext_slack", "slack", "collaboration", egress.OffTeam, "slack.com"),
			sys("ext_jira", "jira", "collaboration", egress.OffService, "jira.example.com:8443"),
			sys("ext_notion", "notion", "collaboration", egress.OffAgent, "notion.so"),
			sys("ext_wiki", "wiki", "collaboration", egress.OffPool, "wiki.example.com"),
			sys("ext_okta", "okta", traffic.IdentityCategory, "", "login.example.com"),
		},
	}
}

// RC-US-5.2: each denied flow is written with the reason that fits why the network denied it.
func TestDeniedReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   string
		port   int
		pool   bool
		reason string
		system string
	}{
		{name: "off at team level", host: "slack.com", reason: traffic.ReasonOffTeam, system: "ext_slack"},
		{name: "off at service level", host: "jira.example.com", port: 8443, reason: traffic.ReasonOffService, system: "ext_jira"},
		{name: "off at agent level", host: "notion.so", reason: traffic.ReasonOffAgent, system: "ext_notion"},
		{name: "off for the pool", host: "wiki.example.com", pool: true, reason: traffic.ReasonOffPool, system: "ext_wiki"},
		{name: "an agent-level system for a pool worker", host: "notion.so", pool: true, reason: traffic.ReasonOffPool, system: "ext_notion"},
		{name: "unknown host", host: "evil.example.net", reason: traffic.ReasonNotInCat},
		{name: "right host, wrong port", host: "jira.example.com", port: 443, reason: traffic.ReasonNotInCat},
		{name: "a system that is on but was denied (policy lag)", host: "api.github.com", reason: traffic.ReasonNotInCat},
		{name: "wildcard matches one label only", host: "a.b.githubusercontent.com", reason: traffic.ReasonNotInCat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			c := newCollector(world(), st, nil)
			c.Handle(t.Context(), denied(func(f *traffic.Flow) {
				f.DstNames = []string{tc.host}
				if tc.port != 0 {
					f.DstPort = tc.port
				}
				if tc.pool {
					f.SrcLabels, f.SrcPod = poolLabels(), "pool-review-7d9f-x2k4q"
				}
			}))
			if err := c.Flush(t.Context()); err != nil || len(st.rows) != 1 {
				t.Fatalf("flush: %v rows=%+v", err, st.rows)
			}
			r := st.rows[0]
			if r.Verdict != traffic.VerdictDenied || r.DeniedReason != tc.reason || r.SystemID != tc.system || r.Connections != 1 || r.TeamID != team {
				t.Errorf("row = %+v", r)
			}
			if tc.pool && (r.PoolID != pool || r.AgentID != "" || r.WorkerPod != "pool-review-7d9f-x2k4q") {
				t.Errorf("pool subject = %+v", r)
			}
			if !tc.pool && (r.AgentID != agtA || r.PoolID != "" || r.WorkerPod != "") {
				t.Errorf("agent subject = %+v", r)
			}
			if !r.BucketStart.Equal(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)) {
				t.Errorf("bucket = %v", r.BucketStart)
			}
		})
	}
}

func TestCounting(t *testing.T) {
	for _, tc := range []struct {
		name string
		fl   traffic.Flow
		want int // connections counted
	}{
		{name: "allowed SYN", fl: flow(nil), want: 1},
		{name: "established packet is not a connection", fl: flow(func(f *traffic.Flow) { f.SYN, f.ACK = false, true }), want: 0},
		{name: "SYN-ACK is not a connection", fl: flow(func(f *traffic.Flow) { f.ACK = true }), want: 0},
		{name: "reply direction", fl: flow(func(f *traffic.Flow) { f.Reply = true }), want: 0},
		{name: "ingress", fl: flow(func(f *traffic.Flow) { f.Egress = false }), want: 0},
		{name: "allowed UDP is not counted", fl: flow(func(f *traffic.Flow) { f.Proto, f.SYN = "udp", false }), want: 0},
		{name: "denied UDP is counted", fl: denied(func(f *traffic.Flow) { f.Proto, f.SYN = "udp", false }), want: 1},
		{name: "other protocol", fl: flow(func(f *traffic.Flow) { f.Proto = "" }), want: 0},
		{name: "dropped for another reason", fl: flow(func(f *traffic.Flow) { f.Forwarded = false }), want: 0},
		{name: "no DNS name", fl: flow(func(f *traffic.Flow) { f.DstNames = nil }), want: 0},
		{name: "in-cluster name", fl: flow(func(f *traffic.Flow) { f.DstNames = []string{"kube-dns.kube-system.svc.cluster.local"} }), want: 0},
		{name: "single label name", fl: flow(func(f *traffic.Flow) { f.DstNames = []string{"localhost"} }), want: 0},
		{name: "trailing dot and case", fl: flow(func(f *traffic.Flow) { f.DstNames = []string{"API.GitHub.com."} }), want: 1},
		{name: "port out of range", fl: flow(func(f *traffic.Flow) { f.DstPort = 0 }), want: 0},
		{name: "no namespace", fl: flow(func(f *traffic.Flow) { f.SrcNamespace = "" }), want: 0},
		{name: "no agent or pool label", fl: flow(func(f *traffic.Flow) { f.SrcLabels = []string{"k8s:app=x"} }), want: 0},
		{name: "both labels", fl: flow(func(f *traffic.Flow) { f.SrcLabels = append(agentLabels(), poolLabels()...) }), want: 0},
		{name: "unprefixed label", fl: flow(func(f *traffic.Flow) { f.SrcLabels = []string{traffic.LabelAgentID + "=" + agtA} }), want: 1},
		{name: "a pool worker with an invalid pod name", fl: flow(func(f *traffic.Flow) { f.SrcLabels, f.SrcPod = poolLabels(), "Bad_Pod" }), want: 0},
		{name: "invalid host name", fl: flow(func(f *traffic.Flow) { f.DstNames = []string{"bad_host.example.com"} }), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			c := newCollector(world(), st, nil)
			c.Handle(t.Context(), tc.fl)
			_ = c.Flush(t.Context())
			got := 0
			for _, r := range st.rows {
				got += r.Connections
			}
			if got != tc.want {
				t.Errorf("connections = %d, want %d (%+v)", got, tc.want, st.rows)
			}
		})
	}
}

// Rows add up per key within a bucket and split across buckets, hosts and verdicts; allowed rows carry
// the system, unmatched hosts none.
func TestAggregation(t *testing.T) {
	st := &fakeStore{}
	c := newCollector(world(), st, nil)
	ctx := t.Context()
	for range 3 {
		c.Handle(ctx, flow(nil))
	}
	c.Handle(ctx, flow(func(f *traffic.Flow) { f.Time = t0.Add(5 * time.Minute) }))
	c.Handle(ctx, flow(func(f *traffic.Flow) { f.DstNames = []string{"files.githubusercontent.com"} }))
	c.Handle(ctx, flow(func(f *traffic.Flow) { f.DstNames = []string{"unknown.example.org"} }))
	c.Handle(ctx, denied(func(f *traffic.Flow) { f.DstNames = []string{"api.github.com"} }))
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	type k struct {
		host, verdict string
		bucket        int
	}
	got := map[k]traffic.Row{}
	for _, r := range st.rows {
		got[k{r.Host, r.Verdict, r.BucketStart.Minute()}] = r
	}
	if len(got) != 5 {
		t.Fatalf("rows = %+v", st.rows)
	}
	if r := got[k{"api.github.com", "allowed", 0}]; r.Connections != 3 || r.SystemID != "ext_gh" || r.DeniedReason != "" {
		t.Errorf("github = %+v", r)
	}
	if r := got[k{"api.github.com", "allowed", 5}]; r.Connections != 1 {
		t.Errorf("next bucket = %+v", r)
	}
	if r := got[k{"files.githubusercontent.com", "allowed", 0}]; r.SystemID != "ext_gh" {
		t.Errorf("wildcard = %+v", r)
	}
	if r := got[k{"unknown.example.org", "allowed", 0}]; r.SystemID != "" || r.Connections != 1 {
		t.Errorf("unmatched = %+v", r)
	}
	for _, r := range st.rows {
		if r.BytesIn != 0 || r.BytesOut != 0 {
			t.Errorf("Hubble flows carry no bytes: %+v", r)
		}
	}
	// Counts are taken: a second flush with nothing new stores nothing.
	before := len(st.rows)
	if err := c.Flush(ctx); err != nil || len(st.rows) != before {
		t.Errorf("second flush stored %d more rows (%v)", len(st.rows)-before, err)
	}
}

// A flush that fails keeps the counts for the next one, without losing or doubling any.
func TestFlushFailureKeepsCounts(t *testing.T) {
	st := &fakeStore{fail: errors.New("db down")}
	c := newCollector(world(), st, nil)
	c.Handle(t.Context(), flow(nil))
	if err := c.Flush(t.Context()); err == nil {
		t.Fatal("want the store's error")
	}
	c.Handle(t.Context(), flow(nil))
	st.fail = nil
	if err := c.Flush(t.Context()); err != nil || len(st.rows) != 1 || st.rows[0].Connections != 2 {
		t.Fatalf("rows = %+v, %v", st.rows, err)
	}
}

// Redaction runs before storage: a host that carries a credential is stored as redacted.invalid, and
// the credential is in no row.
func TestHostRedaction(t *testing.T) {
	st := &fakeStore{}
	c := newCollector(world(), st, nil)
	c.Handle(t.Context(), denied(func(f *traffic.Flow) { f.DstNames = []string{"akiaiosfodnn7example.evil.example.net"} }))
	_ = c.Flush(t.Context())
	if len(st.rows) != 1 || st.rows[0].Host != traffic.RedactedHost || st.rows[0].DeniedReason != traffic.ReasonNotInCat {
		t.Fatalf("rows = %+v", st.rows)
	}
	for _, r := range st.rows {
		if strings.Contains(strings.ToLower(r.Host), "akia") {
			t.Errorf("credential stored: %+v", r)
		}
	}
}

// A flow's labels are a claim; the directory decides. A pod that claims another team's agent, or an
// agent that does not exist, is not counted, and a directory failure drops the flow instead of
// guessing.
func TestSourceMustBeVerified(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*traffic.Flow)
		dir  func(*fakeDir)
	}{
		{name: "agent in another namespace", mod: func(f *traffic.Flow) { f.SrcNamespace = "afs-t-other" }},
		{name: "unknown agent", dir: func(d *fakeDir) { d.known = map[string]bool{} }},
		{name: "directory failure", dir: func(d *fakeDir) { d.err = errors.New("db down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, st := world(), &fakeStore{}
			if tc.dir != nil {
				tc.dir(d)
			}
			c := newCollector(d, st, nil)
			c.Handle(t.Context(), flow(tc.mod))
			_ = c.Flush(t.Context())
			if len(st.rows) != 0 {
				t.Errorf("rows = %+v", st.rows)
			}
		})
	}
}

// A resolved source is cached for CacheTTL: a system switched off shows up after it expires.
func TestDirectoryCache(t *testing.T) {
	now := t0
	d, st := world(), &fakeStore{}
	c := newCollector(d, st, func(cfg *traffic.Config) { cfg.Now = func() time.Time { return now }; cfg.CacheTTL = time.Minute })
	for range 3 {
		c.Handle(t.Context(), flow(nil))
	}
	if d.calls != 1 {
		t.Errorf("lookups = %d, want 1 within the TTL", d.calls)
	}
	now = now.Add(2 * time.Minute)
	c.Handle(t.Context(), flow(nil))
	if d.calls != 2 {
		t.Errorf("lookups = %d, want 2 after the TTL", d.calls)
	}
}

// RC-US-16.5: a connection to an identity system's endpoint is handed to the sign-in sink (agent or
// worker, system, time) and only an allowed connection to a system that is on is.
func TestSignIns(t *testing.T) {
	login := func(mod func(*traffic.Flow)) traffic.Flow {
		return flow(func(f *traffic.Flow) {
			f.DstNames = []string{"login.example.com"}
			if mod != nil {
				mod(f)
			}
		})
	}
	for _, tc := range []struct {
		name string
		fl   traffic.Flow
		want int
	}{
		{name: "agent sign-in", fl: login(nil), want: 1},
		{name: "pool worker sign-in", fl: login(func(f *traffic.Flow) { f.SrcLabels, f.SrcPod = poolLabels(), "pool-review-7d9f-x2k4q" }), want: 1},
		{name: "not the first packet", fl: login(func(f *traffic.Flow) { f.SYN, f.ACK = false, true }), want: 0},
		{name: "a denied connection is not a sign-in", fl: denied(func(f *traffic.Flow) { f.DstNames = []string{"login.example.com"} }), want: 0},
		{name: "another category", fl: flow(nil), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			si, st := &recordedSignIns{}, &fakeStore{}
			c := newCollector(world(), st, func(cfg *traffic.Config) { cfg.SignIns = si })
			c.Handle(t.Context(), tc.fl)
			if len(si.got) != tc.want {
				t.Fatalf("sign-ins = %+v, want %d", si.got, tc.want)
			}
			if tc.want == 1 {
				g := si.got[0]
				if g.TeamID != team || g.Namespace != ns || g.System != "okta" || g.SystemID != "ext_okta" || !g.At.Equal(t0) || g.Host != "login.example.com" {
					t.Errorf("sign-in = %+v", g)
				}
				if (g.AgentID == "") == (g.PoolID == "") {
					t.Errorf("exactly one of agent and pool: %+v", g)
				}
			}
		})
	}
}

// A sign-in that cannot be delivered (the team has no log delivery) is not an error and is stored
// nowhere: the sink is the only place it goes.
func TestSignInNotDeliveredIsDropped(t *testing.T) {
	si, st := &recordedSignIns{err: errors.New("no aggregator")}, &fakeStore{}
	c := newCollector(world(), st, func(cfg *traffic.Config) { cfg.SignIns = si })
	c.Handle(t.Context(), flow(func(f *traffic.Flow) { f.DstNames = []string{"login.example.com"} }))
	if len(si.got) != 1 {
		t.Fatal("sink not called")
	}
	_ = c.Flush(t.Context())
	// The connection is traffic like any other (a count), never a per-sign-in record.
	if len(st.rows) != 1 || st.rows[0].Connections != 1 || st.rows[0].Host != "login.example.com" {
		t.Errorf("rows = %+v", st.rows)
	}
}

type fakeSource struct {
	mu      sync.Mutex
	flows   []traffic.Flow
	streams int
}

func (s *fakeSource) Stream(ctx context.Context, h func(traffic.Flow)) error {
	s.mu.Lock()
	s.streams++
	n, flows := s.streams, s.flows
	s.mu.Unlock()
	if n == 1 {
		for _, f := range flows {
			h(f)
		}
		return errors.New("stream reset")
	}
	<-ctx.Done()
	return ctx.Err()
}

// Run reconnects after a broken stream and stores what it counted when the context ends.
func TestRun(t *testing.T) {
	src, st, obs := &fakeSource{flows: []traffic.Flow{flow(nil), flow(nil)}}, &fakeStore{}, &recordedObserver{}
	c := newCollector(world(), st, func(cfg *traffic.Config) {
		cfg.Source = src
		cfg.Observer = obs
		cfg.FlushEvery = time.Hour
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		src.mu.Lock()
		n := src.streams
		src.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 || st.rows[0].Connections != 2 || len(obs.batches) != 1 {
		t.Fatalf("rows = %+v batches=%d", st.rows, len(obs.batches))
	}
}

func TestRunFlushesPeriodically(t *testing.T) {
	src, st := &fakeSource{flows: []traffic.Flow{flow(nil)}}, &fakeStore{}
	c := newCollector(world(), st, func(cfg *traffic.Config) { cfg.Source = src; cfg.FlushEvery = 10 * time.Millisecond })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		n := len(st.rows)
		st.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if len(st.rows) != 1 {
		t.Fatalf("rows = %+v", st.rows)
	}
}

func TestRowsAreSorted(t *testing.T) {
	st := &fakeStore{}
	c := newCollector(world(), st, nil)
	for _, h := range []string{"b.example.org", "a.example.org"} {
		c.Handle(t.Context(), flow(func(f *traffic.Flow) { f.DstNames = []string{h} }))
	}
	c.Handle(t.Context(), flow(func(f *traffic.Flow) { f.DstNames = []string{"a.example.org"}; f.DstPort = 80 }))
	c.Handle(t.Context(), denied(func(f *traffic.Flow) { f.DstNames = []string{"a.example.org"} }))
	c.Handle(t.Context(), flow(func(f *traffic.Flow) { f.SrcLabels, f.SrcPod = poolLabels(), "w-1" }))
	c.Handle(t.Context(), flow(func(f *traffic.Flow) { f.Time = t0.Add(-time.Hour) }))
	_ = c.Flush(t.Context())
	var got []string
	for _, r := range st.rows {
		got = append(got, r.BucketStart.Format("15:04")+" "+r.AgentID+r.PoolID+" "+r.Host+" "+r.Verdict)
	}
	if !reflect.DeepEqual(got[:2], []string{"09:00 " + agtA + " api.github.com allowed", "10:00 " + agtA + " a.example.org allowed"}) {
		t.Errorf("order = %v", got)
	}
}
