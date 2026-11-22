package edgelog

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf, func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) })
	w.Write(edge.Event{TeamID: "team_01", Event: "denied", PrincipalKind: "user", PrincipalID: "sub-1", Method: "POST", Route: "/api", Status: 403,
		Reason: "permission_required", Duration: 12 * time.Millisecond, ServiceAdminEntry: true, RequestID: "req_1"})
	w.Write(edge.Event{TeamID: "team_01", Event: "sign_in", PrincipalKind: "user", Method: "GET", Route: "/_afs", Status: 302})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"afs.team_id": "team_01", "event": "denied", "level": "WARN", "status": 403.0, "durationMs": 12.0,
		"serviceAdminEntry": true, "service": "afs-edge", "requestId": "req_1", "route": "/api", "time": "2026-11-22T12:00:00Z"} {
		if m[k] != want {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	if err := json.Unmarshal([]byte(lines[1]), &m); err != nil || m["level"] != "INFO" {
		t.Errorf("%v %v", m, err)
	}
	// the record has no field that could hold a body, header, cookie, token or query
	for _, banned := range []string{"query", "cookie", "token", "body", "header", "authorization"} {
		if strings.Contains(strings.ToLower(lines[0]), banned) {
			t.Errorf("record mentions %s", banned)
		}
	}
	New(&buf, nil).Write(edge.Event{})
}
