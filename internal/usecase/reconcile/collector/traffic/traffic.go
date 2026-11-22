// Package traffic is the external traffic collector of the reconciler (RC-7, RC-US-5.2 to 5.3,
// RC-US-16.5). It turns the network flows of agents and pool workers into external_traffic rows:
// per 5-minute bucket, agent (or pool and worker pod), host, port and verdict, the connections, with the
// reason when the network denied them and the tags of the team at the time.
//
// Flows come from Hubble (adapter/k8s/hubble) through the FlowSource port. Hubble sees layer 3 and 4
// only, so bytes stay zero and requests and errors stay empty (OQ-8): they are filled only where an
// egress proxy exists. A flow is counted once, when its connection starts (a TCP SYN without ACK), or
// once per dropped datagram.
//
// What a flow's source is comes from the source pod's labels, but a label is only a claim: the
// Directory port checks the claimed agent or pool against the database and the pod's namespace, and a
// flow whose claim does not hold is dropped. Host names pass the redaction filter before anything is
// stored (a name can carry a credential, for example in a DNS exfiltration attempt).
//
// A connection to the sign-in endpoint of an identity system is also handed to the SignIns port, which
// delivers it to the team's log delivery and nowhere else: it is never a table row of its own and never
// an event. The connection counts in external_traffic like any other connection.
package traffic

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

// BucketSize is the width of a traffic bucket.
const BucketSize = 5 * time.Minute

// RedactedHost is stored in place of a host name the redaction filter changed.
const RedactedHost = "redacted.invalid"

// Labels the collector reads from a flow source's pod. They are set by the agent and pool renderers.
const (
	LabelAgentID = "afs.stainedhead.io/agent-id"
	LabelPoolID  = "afs.stainedhead.io/pool-id"
)

// Verdicts of a row.
const (
	VerdictAllowed = "allowed"
	VerdictDenied  = "denied"
)

// Denied reasons of a row (database contract: external_traffic.denied_reason).
const (
	ReasonOffService = "system_off_service"
	ReasonOffTeam    = "system_off_team"
	ReasonOffAgent   = "system_off_agent"
	ReasonOffPool    = "system_off_pool"
	ReasonNotInCat   = "not_in_catalog"
)

// IdentityCategory is the category of the external systems whose connections are sign-ins.
const IdentityCategory = "identity"

// Flow is one observed network flow, reduced to what the collector needs.
type Flow struct {
	Time time.Time
	// Egress is true for traffic leaving the source pod.
	Egress bool
	// Reply is true for the reply direction of a connection.
	Reply bool
	// Forwarded is true when the network let the flow through.
	Forwarded bool
	// PolicyDenied is true when the network dropped the flow because no policy allowed it.
	PolicyDenied bool
	// Proto is "tcp" or "udp" ("" for anything else).
	Proto   string
	SYN     bool
	ACK     bool
	DstPort int
	// DstNames are the DNS names the destination was resolved from, if any.
	DstNames []string
	// Source pod.
	SrcNamespace string
	SrcPod       string
	// SrcLabels are the source pod's labels as "key=value", optionally prefixed by their source
	// ("k8s:").
	SrcLabels []string
}

// FlowSource streams flows until ctx ends or the stream breaks.
type FlowSource interface {
	Stream(ctx context.Context, handle func(Flow)) error
}

// Resolved is what the collector knows about a verified flow source.
type Resolved struct {
	TeamID string
	// Systems are the external systems of the source's view, with the level each is off at.
	Systems []egress.System
}

// Directory verifies a claimed source and returns its team and systems.
type Directory interface {
	// ResolveAgent returns ok=false when the agent does not exist or does not live in namespace.
	ResolveAgent(ctx context.Context, namespace, agentID string) (Resolved, bool, error)
	// ResolvePool is ResolveAgent for a pool.
	ResolvePool(ctx context.Context, namespace, poolID string) (Resolved, bool, error)
}

// Row is a delta to add to external_traffic: the row for the key is created, or its counters are
// increased.
type Row struct {
	BucketStart time.Time
	TeamID      string
	AgentID     string
	PoolID      string
	WorkerPod   string
	// SystemID is "" for a host that matches no system.
	SystemID     string
	Host         string
	Port         int
	Verdict      string
	DeniedReason string
	Connections  int
	BytesOut     int64
	BytesIn      int64
}

// Store adds rows to external_traffic, stamping each with the team's current tag set.
type Store interface {
	Add(ctx context.Context, rows []Row) error
}

// Redactor is the redaction filter (usecase/redact).
type Redactor interface {
	Redact(s string) string
}

// SignIn is a connection to the sign-in endpoint of an identity system.
type SignIn struct {
	At        time.Time
	TeamID    string
	Namespace string
	AgentID   string
	PoolID    string
	WorkerPod string
	SystemID  string
	System    string
	Host      string
}

// SignIns delivers sign-in events to the team's log delivery, and nowhere else.
type SignIns interface {
	Record(ctx context.Context, s SignIn) error
}

// Observer is told of every batch of rows that was stored (the spike detector).
type Observer interface {
	Observe(ctx context.Context, rows []Row)
}

// Config configures the Collector.
type Config struct {
	Source    FlowSource
	Directory Directory
	Store     Store
	Redactor  Redactor
	// SignIns and Observer are optional.
	SignIns  SignIns
	Observer Observer
	// FlushEvery is how often counts are stored (default 30 s).
	FlushEvery time.Duration
	// CacheTTL is how long a resolved source is trusted (default 30 s): a system switched off is seen
	// within this time.
	CacheTTL time.Duration
	Now      func() time.Time
	Log      *slog.Logger
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
var workerPodRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,251}[a-z0-9])?$`)

type key struct {
	bucket                  time.Time
	agent, pool, worker     string
	host                    string
	port                    int
	verdict, reason, system string
}

type cached struct {
	res     Resolved
	ok      bool
	expires time.Time
}

// Collector aggregates flows and stores them.
type Collector struct {
	cfg Config

	mu     sync.Mutex
	counts map[key]int
	teams  map[key]string
	cache  map[string]cached
}

// New returns a Collector. Source, Directory, Store and Redactor are required.
func New(cfg Config) *Collector {
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 30 * time.Second
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Collector{cfg: cfg, counts: map[key]int{}, teams: map[key]string{}, cache: map[string]cached{}}
}

// Run streams and flushes until ctx ends, reconnecting with backoff when the stream breaks. It stores
// what it counted before it returns.
func (c *Collector) Run(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(c.cfg.FlushEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.Flush(ctx); err != nil {
					c.cfg.Log.Warn("traffic flush failed", slog.Any("err", err))
				}
			}
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.cfg.Source.Stream(ctx, func(f Flow) { c.Handle(ctx, f) })
		if ctx.Err() != nil {
			break
		}
		c.cfg.Log.Warn("flow stream ended", slog.Any("err", err))
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	<-done
	// The context is over; the last counts are stored with a fresh, short one.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return c.Flush(fctx)
}

// externalHost returns the first name of names that is not inside the cluster.
func externalHost(names []string) string {
	for _, n := range names {
		n = strings.TrimSuffix(strings.ToLower(n), ".")
		if n == "" || strings.HasSuffix(n, ".cluster.local") || strings.HasSuffix(n, ".svc") || !strings.Contains(n, ".") {
			continue
		}
		return n
	}
	return ""
}

// labelValue finds a label of the pod, whatever the prefix before its key.
func labelValue(labels []string, key string) string {
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		if k == key || strings.HasSuffix(k, ":"+key) {
			return v
		}
	}
	return ""
}

// Handle counts one flow.
func (c *Collector) Handle(ctx context.Context, f Flow) {
	if !f.Egress || f.Reply {
		return
	}
	var verdict string
	switch {
	case f.Forwarded:
		verdict = VerdictAllowed
	case f.PolicyDenied:
		verdict = VerdictDenied
	default:
		return
	}
	// One count per connection: the SYN that opens it. A datagram has no handshake, so only denied ones
	// are counted (every forwarded packet of a stream would otherwise be a connection).
	switch f.Proto {
	case "tcp":
		if !f.SYN || f.ACK {
			return
		}
	case "udp":
		if verdict != VerdictDenied {
			return
		}
	default:
		return
	}
	raw := externalHost(f.DstNames)
	if raw == "" || f.DstPort < 1 || f.DstPort > 65535 {
		return
	}
	agentID, poolID := labelValue(f.SrcLabels, LabelAgentID), labelValue(f.SrcLabels, LabelPoolID)
	if f.SrcNamespace == "" || (agentID == "") == (poolID == "") {
		return
	}
	res, ok, err := c.resolve(ctx, f.SrcNamespace, agentID, poolID)
	if err != nil {
		c.cfg.Log.Warn("traffic source lookup failed", slog.Any("err", err))
		return
	}
	if !ok {
		return
	}
	k := key{bucket: c.bucket(f.Time), host: raw, port: f.DstPort, verdict: verdict, agent: agentID, pool: poolID}
	if poolID != "" {
		if !workerPodRE.MatchString(f.SrcPod) {
			return
		}
		k.worker = f.SrcPod
	}
	// Redaction first: nothing derived from the name outlives this line unredacted.
	host := raw
	if c.cfg.Redactor.Redact(raw) != raw {
		host = RedactedHost
	}
	if !hostRE.MatchString(host) {
		return
	}
	k.host = host

	sys := match(res.Systems, raw, f.DstPort)
	if sys != nil {
		k.system = sys.ID
	}
	if verdict == VerdictDenied {
		k.reason = reasonFor(sys, poolID != "")
		if k.reason == ReasonNotInCat {
			k.system = ""
		}
	}

	c.mu.Lock()
	c.counts[k]++
	c.teams[k] = res.TeamID
	c.mu.Unlock()

	if c.cfg.SignIns != nil && verdict == VerdictAllowed && sys != nil && sys.Category == IdentityCategory && sys.DisabledAt == "" {
		si := SignIn{At: f.Time, TeamID: res.TeamID, Namespace: f.SrcNamespace, AgentID: agentID, PoolID: poolID, WorkerPod: k.worker,
			SystemID: sys.ID, System: sys.Slug, Host: host}
		if si.At.IsZero() {
			si.At = c.cfg.Now()
		}
		if err := c.cfg.SignIns.Record(ctx, si); err != nil {
			c.cfg.Log.Debug("sign-in not delivered", slog.String("team", res.TeamID), slog.Any("err", err))
		}
	}
}

func (c *Collector) bucket(t time.Time) time.Time {
	if t.IsZero() {
		t = c.cfg.Now()
	}
	return t.UTC().Truncate(BucketSize)
}

func (c *Collector) resolve(ctx context.Context, ns, agentID, poolID string) (Resolved, bool, error) {
	ck := "a:" + ns + "/" + agentID
	if poolID != "" {
		ck = "p:" + ns + "/" + poolID
	}
	now := c.cfg.Now()
	c.mu.Lock()
	e, hit := c.cache[ck]
	c.mu.Unlock()
	if hit && now.Before(e.expires) {
		return e.res, e.ok, nil
	}
	var (
		res Resolved
		ok  bool
		err error
	)
	if poolID != "" {
		res, ok, err = c.cfg.Directory.ResolvePool(ctx, ns, poolID)
	} else {
		res, ok, err = c.cfg.Directory.ResolveAgent(ctx, ns, agentID)
	}
	if err != nil {
		return Resolved{}, false, err
	}
	c.mu.Lock()
	c.cache[ck] = cached{res: res, ok: ok, expires: now.Add(c.cfg.CacheTTL)}
	if len(c.cache) > 10000 {
		c.cache = map[string]cached{ck: c.cache[ck]}
	}
	c.mu.Unlock()
	return res, ok, nil
}

// match finds the system of a view that lists host:port. A system that is on wins over one that is off
// when two list the same endpoint.
func match(systems []egress.System, host string, port int) *egress.System {
	var found *egress.System
	for i := range systems {
		s := &systems[i]
		for _, raw := range s.Endpoints {
			e, err := egress.ParseEndpoint(raw)
			if err != nil || e.Port != port || !hostMatches(e, host) {
				continue
			}
			if s.DisabledAt == "" {
				return s
			}
			if found == nil {
				found = s
			}
		}
	}
	return found
}

// hostMatches applies the policy's semantics: a name is exact, "*.domain" is one label below domain.
func hostMatches(e egress.Endpoint, host string) bool {
	if !e.Wildcard() {
		return e.Host == host
	}
	suffix := e.Host[1:] // ".domain"
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := strings.TrimSuffix(host, suffix)
	return label != "" && !strings.Contains(label, ".")
}

// reasonFor says why the network denied a flow to a host that matched sys (nil: no system).
func reasonFor(sys *egress.System, pool bool) string {
	if sys == nil || sys.DisabledAt == "" {
		// No system, or one that is on yet was denied (the policy had not caught up): the host is not
		// in what the source may reach.
		return ReasonNotInCat
	}
	switch sys.DisabledAt {
	case egress.OffService:
		return ReasonOffService
	case egress.OffTeam:
		return ReasonOffTeam
	case egress.OffPool:
		return ReasonOffPool
	default:
		if pool {
			return ReasonOffPool
		}
		return ReasonOffAgent
	}
}

// Flush stores the counts since the last flush. On failure the counts are kept and added to the next
// flush, so nothing is lost or counted twice.
func (c *Collector) Flush(ctx context.Context) error {
	c.mu.Lock()
	counts, teams := c.counts, c.teams
	c.counts, c.teams = map[key]int{}, map[key]string{}
	c.mu.Unlock()
	if len(counts) == 0 {
		return nil
	}
	rows := make([]Row, 0, len(counts))
	for k, n := range counts {
		rows = append(rows, Row{BucketStart: k.bucket, TeamID: teams[k], AgentID: k.agent, PoolID: k.pool, WorkerPod: k.worker,
			SystemID: k.system, Host: k.host, Port: k.port, Verdict: k.verdict, DeniedReason: k.reason, Connections: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case !a.BucketStart.Equal(b.BucketStart):
			return a.BucketStart.Before(b.BucketStart)
		case a.AgentID+a.PoolID+a.WorkerPod != b.AgentID+b.PoolID+b.WorkerPod:
			return a.AgentID+a.PoolID+a.WorkerPod < b.AgentID+b.PoolID+b.WorkerPod
		case a.Host != b.Host:
			return a.Host < b.Host
		case a.Port != b.Port:
			return a.Port < b.Port
		}
		return a.Verdict < b.Verdict
	})
	if err := c.cfg.Store.Add(ctx, rows); err != nil {
		c.mu.Lock()
		for k, n := range counts {
			c.counts[k] += n
			c.teams[k] = teams[k]
		}
		c.mu.Unlock()
		return err
	}
	if c.cfg.Observer != nil {
		c.cfg.Observer.Observe(ctx, rows)
	}
	return nil
}
