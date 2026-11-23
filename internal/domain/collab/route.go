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

// Level is the entry level of a hostname: the public edge or the private (internal) edge.
type Level string

// Levels, as afs.orchestration_routes() names them.
const (
	LevelPublic  Level = "public"
	LevelPrivate Level = "private"
)

// Other is the other level.
func (l Level) Other() Level {
	if l == LevelPrivate {
		return LevelPublic
	}
	return LevelPrivate
}

// Route is a team's endpoint as served on one hostname: afs.orchestration_routes() gives a team two rows (Web UI
// and Web API) and the edge registers the team under its public and its private hostname, so one team yields
// up to two Routes, one per Level. WebUIEffective and RESTAPIEffective mean "effective at this Level"; the
// *OtherLevel flags say the surface is effective at the other level, so on this hostname it answers 404 (CD-38).
type Route struct {
	TeamID, Slug, Namespace, Hostname, TeamState string
	// Level is the entry level of Hostname. PublicHostname and PrivateHostname are the team's hostnames at
	// both levels (empty when that level has no base domain).
	Level                            Level
	PublicHostname, PrivateHostname  string
	Tool                             string
	AgentID, AgentState              string
	WebUIEnabled, RESTAPIEnabled     bool
	WebUIEffective, RESTAPIEffective bool
	// WebUIOtherLevel and RESTAPIOtherLevel: the surface is effective at the other level.
	WebUIOtherLevel, RESTAPIOtherLevel bool
	MayExpose, Ready                   bool
	NotReadyReason                     string
	ServiceName                        string
	Port                               int
	ProviderID, IssuerURL, GroupsClaim string
	ClientID, ClientSecretARN          string
	// ProviderClientID is the provider's console client id and ProviderCLIClientID its CLI public client id
	// (empty when none): the audiences afs-api accepts, returned by collab_routes() since -db 000021 (RD-27).
	ProviderClientID, ProviderCLIClientID string
}

// Upstream is the only address the edge forwards a team's traffic to.
func (r Route) Upstream() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", r.ServiceName, r.Namespace, r.Port)
}

// Effective reports whether the surface is effective for the team at this Route's level.
func (r Route) Effective(s Surface) bool {
	if s == SurfaceWebUI {
		return r.WebUIEffective
	}
	return r.RESTAPIEffective
}

// OtherLevel reports whether the surface is effective at the other level only: this hostname answers 404.
func (r Route) OtherLevel(s Surface) bool {
	if s == SurfaceWebUI {
		return r.WebUIOtherLevel
	}
	return r.RESTAPIOtherLevel
}

// ExposureLabel is the exposure the access records carry for this Route ("public" or "private").
func (r Route) ExposureLabel() string {
	if r.Level == LevelPrivate {
		return string(LevelPrivate)
	}
	return string(LevelPublic)
}

// Settings are the service settings the edge reads: afs.orchestration_settings() (domains and switches) and
// afs.collab_settings() (the rest).
type Settings struct {
	// PublicBaseDomain and PrivateBaseDomain are orchestration.public_base_domain and
	// orchestration.private_base_domain; PublicEndpoints and PrivateEndpoints the two service switches.
	PublicBaseDomain     string
	PrivateBaseDomain    string
	PublicEndpoints      bool
	PrivateEndpoints     bool
	AllowedSourceCIDRs   []string
	SessionTTLMinutes    int
	ContentScan          string // block, warn or off
	MaxBodyMB            int
	DeniedSpikeThreshold int
}

// DefaultSettings are the contract defaults.
func DefaultSettings() Settings {
	return Settings{PublicEndpoints: true, PrivateEndpoints: true, SessionTTLMinutes: 480, ContentScan: "block", MaxBodyMB: 25, DeniedSpikeThreshold: 50}
}

// BaseDomain is the base domain of a level (empty when unset).
func (s Settings) BaseDomain(l Level) string {
	if l == LevelPrivate {
		return s.PrivateBaseDomain
	}
	return s.PublicBaseDomain
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
	if d := t.settings.PublicBaseDomain; d != "" && host == strings.ToLower(d) {
		return Route{}, HostApex
	}
	if d := t.settings.PrivateBaseDomain; d != "" && host == strings.ToLower(d) {
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
