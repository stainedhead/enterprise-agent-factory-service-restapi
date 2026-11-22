package collab

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Surface is a way into a team's Paperclip.
type Surface string

// Surfaces.
const (
	SurfaceWebUI   Surface = "web_ui"
	SurfaceRESTAPI Surface = "rest_api"
)

// PrincipalKind is who made a request.
type PrincipalKind string

// Principal kinds, as afs.record_collab_access takes them.
const (
	KindUser      PrincipalKind = "user"
	KindAPIKey    PrincipalKind = "api_key"
	KindAnonymous PrincipalKind = "anonymous"
)

// Route is one row of afs.collab_routes(): everything the edge knows of a team's endpoint.
type Route struct {
	TeamID, Slug, Namespace, Hostname, TeamState string
	AgentID, AgentState                          string
	WebUIEnabled, RESTAPIEnabled                 bool
	WebUIEffective, RESTAPIEffective             bool
	MayExpose, Ready                             bool
	NotReadyReason                               string
	ServiceName                                  string
	Port                                         int
	ProviderID, IssuerURL, GroupsClaim           string
	ClientID, ClientSecretARN                    string
	// ProviderClientID is the provider's own client id, the audience afs-api accepts. Empty until the
	// database reports it (see implementation notes, RD-27).
	ProviderClientID string
}

// Upstream is the only address the edge forwards a team's traffic to.
func (r Route) Upstream() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", r.ServiceName, r.Namespace, r.Port)
}

// Effective reports whether the surface is effective for the team.
func (r Route) Effective(s Surface) bool {
	if s == SurfaceWebUI {
		return r.WebUIEffective
	}
	return r.RESTAPIEffective
}

// Settings are the collab.* service settings the edge reads (afs.collab_settings()).
type Settings struct {
	BaseDomain           string
	PublicEndpoints      bool
	AllowedSourceCIDRs   []string
	SessionTTLMinutes    int
	ContentScan          string // block, warn or off
	MaxBodyMB            int
	DeniedSpikeThreshold int
}

// DefaultSettings are the contract defaults.
func DefaultSettings() Settings {
	return Settings{PublicEndpoints: true, SessionTTLMinutes: 480, ContentScan: "block", MaxBodyMB: 25, DeniedSpikeThreshold: 50}
}

// MaxBodyBytes is the body limit.
func (s Settings) MaxBodyBytes() int64 { return int64(s.MaxBodyMB) << 20 }

// Table is the edge's route table: the last good load, kept up to MaxStale when a refresh fails.
type Table struct {
	mu       sync.RWMutex
	byHost   map[string]Route
	settings Settings
	loaded   time.Time
	now      func() time.Time
}

// MaxStale is how long a failed refresh keeps the last table (EDGE-2).
const MaxStale = 5 * time.Minute

// NewTable returns an empty table; now is the clock (time.Now when nil).
func NewTable(now func() time.Time) *Table {
	if now == nil {
		now = time.Now
	}
	return &Table{byHost: map[string]Route{}, settings: DefaultSettings(), now: now}
}

// Replace installs a freshly loaded table and settings.
func (t *Table) Replace(routes []Route, s Settings) {
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		if r.Hostname != "" {
			m[strings.ToLower(r.Hostname)] = r
		}
	}
	t.mu.Lock()
	t.byHost, t.settings, t.loaded = m, s, t.now()
	t.mu.Unlock()
}

// Fresh reports whether the table is usable: loaded and not stale for more than MaxStale.
func (t *Table) Fresh() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return !t.loaded.IsZero() && t.now().Sub(t.loaded) <= MaxStale
}

// Settings returns the settings of the last load.
func (t *Table) Settings() Settings {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.settings
}

// Host classifies a Host header: the apex, a known team host, or unknown. The port is ignored.
type HostKind int

// Host kinds.
const (
	HostUnknown HostKind = iota
	HostApex
	HostTeam
)

// Lookup resolves a Host header.
func (t *Table) Lookup(host string) (Route, HostKind) {
	host = strings.ToLower(host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.settings.BaseDomain != "" && host == strings.ToLower(t.settings.BaseDomain) {
		return Route{}, HostApex
	}
	if r, ok := t.byHost[host]; ok {
		return r, HostTeam
	}
	return Route{}, HostUnknown
}

// RouteByTeam returns the route of a team id.
func (t *Table) RouteByTeam(teamID string) (Route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, r := range t.byHost {
		if r.TeamID == teamID {
			return r, true
		}
	}
	return Route{}, false
}
