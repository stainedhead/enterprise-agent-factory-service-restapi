// Package mcpgateway is the MCP route of the team gateway (gateway contract §13): the aggregated
// endpoint /mcp and the per-server endpoints /mcp/{server}. It depends only on ports, so afs-gateway
// mounts it on the team gateway's mux and supplies identity, admission, upstream calls and the ledger.
package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

// Caller is the identified agent or pool behind a request.
type Caller struct {
	TeamID      string
	SubjectKind string // agent or pool
	SubjectID   string
	WorkerPod   string
}

// Identify returns the caller of a request, from the team gateway's identity rules (gateway contract §3).
type Identify func(*http.Request) (Caller, error)

// Server is one server in the caller's effective set.
type Server struct {
	ID       string
	Slug     string
	Kind     string // enterprise or team
	ReadOnly bool
	Limits   Limits
}

// Limits are per-server request limits (gateway contract §13.2).
type Limits struct {
	MaxRequestBytes  int64
	MaxResponseBytes int64
	Timeout          time.Duration
	Concurrency      int
}

// Entry is one primitive of an effective server with its pinned definition and the admission decision.
type Entry struct {
	ServerID   string
	Primitive  mcp.Primitive
	Definition json.RawMessage // the approved definition, as the upstream sent it (tool, resource, template or prompt object)
	Digest     string
	Decision   mcp.Decision
	Reason     mcp.Reason // set when the decision is a refusal
}

// Snapshot is the caller's effective MCP view at one moment, from afs.mcp_admission (cached ≤ 60 s).
type Snapshot struct {
	Version string // changes whenever the view changes; keys the search index cache
	Mode    string // search or full
	Servers []Server
	Entries []Entry
}

// Admission returns the effective view of a caller.
type Admission interface {
	Snapshot(ctx context.Context, c Caller) (*Snapshot, error)
}

// Upstream forwards one request to a server. The adapter owns sessions, versions and credentials; it
// never forwards the agent's own credentials.
type Upstream interface {
	Call(ctx context.Context, server Server, m *mcpwire.Message) (*mcpwire.Message, error)
}

// Event is one ledger row (database contract §7.5.4), with no content.
type Event struct {
	At            time.Time
	RequestID     string
	Caller        Caller
	ServerID      string
	ServerKind    string
	Method        string
	Name          string
	Digest        string
	Outcome       string
	Reason        string
	JSONRPCCode   int
	HTTPStatus    int
	GatewayMS     int
	UpstreamMS    int
	RequestBytes  int
	ResponseBytes int
	Protocol      string
	SearchResults []string
	SearchHit     *bool
	QueryText     string // only when a capture window is open; the database drops it otherwise
}

// Ledger records events. It must not block the call; failures are the ledger's to report.
type Ledger interface {
	Record(Event)
}

// Outcomes.
const (
	OutcomeOK                  = "ok"
	OutcomeToolError           = "tool_error"
	OutcomeProtocolError       = "protocol_error"
	OutcomeRefused             = "refused"
	OutcomeUpstreamUnavailable = "upstream_unavailable"
	OutcomeTimeout             = "timeout"
	OutcomeCancelled           = "cancelled"
)
