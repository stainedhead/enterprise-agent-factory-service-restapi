// Package edgelog writes afs-edge's records: one JSON line per sign-in, sign-out and denied request, with the
// field afs.team_id so the node log collector can route it to the team's aggregator (COL-31). A record holds
// no body, header value, cookie, token or query string, and its route is the first path segment only.
package edgelog

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// Writer implements edge.Records.
type Writer struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

var _ edge.Records = (*Writer)(nil)

// New returns a Writer on w (stdout in production). now defaults to time.Now.
func New(w io.Writer, now func() time.Time) *Writer {
	if now == nil {
		now = time.Now
	}
	return &Writer{enc: json.NewEncoder(w), now: now}
}

type line struct {
	Time              string `json:"time"`
	Level             string `json:"level"`
	Service           string `json:"service"`
	Msg               string `json:"msg"`
	TeamID            string `json:"afs.team_id"`
	Event             string `json:"event"`
	PrincipalKind     string `json:"principalKind"`
	PrincipalID       string `json:"principalId,omitempty"`
	Method            string `json:"method"`
	Route             string `json:"route"`
	Status            int    `json:"status"`
	Reason            string `json:"reason,omitempty"`
	DurationMs        int64  `json:"durationMs"`
	ServiceAdminEntry bool   `json:"serviceAdminEntry,omitempty"`
	RequestID         string `json:"requestId"`
}

// Write implements edge.Records.
func (w *Writer) Write(e edge.Event) {
	l := line{Time: w.now().UTC().Format(time.RFC3339Nano), Level: "INFO", Service: "afs-edge", Msg: "collaboration " + e.Event,
		TeamID: e.TeamID, Event: e.Event, PrincipalKind: e.PrincipalKind, PrincipalID: e.PrincipalID, Method: e.Method, Route: e.Route,
		Status: e.Status, Reason: e.Reason, DurationMs: e.Duration.Milliseconds(), ServiceAdminEntry: e.ServiceAdminEntry, RequestID: e.RequestID}
	if e.Event == "denied" {
		l.Level = "WARN"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(l)
}
