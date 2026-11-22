package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// trickle sends a request whose body arrives in two parts, pause apart, and returns the response (or the
// error that ended the exchange) and how long it took.
func trickle(t *testing.T, addr, path, token, body string, pause time.Duration) (code int, out string, took time.Duration, err error) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	start := time.Now()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	half := len(body) / 2
	_, _ = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		path, token, len(body), body[:half])
	time.Sleep(pause)
	_, _ = io.WriteString(conn, body[half:]) // may fail once the server has closed the connection
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, "", time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), time.Since(start), nil
}

// D-6 (FR-R09): a body that trickles in slower than AFS_HTTP_READ_TIMEOUT is cut off, except on
// CreateImport, which extends its own read deadline to 10 minutes.
func TestSlowBodyIsBoundedExceptImport(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	s := settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6,
		HTTPReadTimeout: 300 * time.Millisecond, HTTPIdleTimeout: time.Second}
	c, err := wire(t.Context(), quiet, s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ts := httptest.NewUnstartedServer(c.Handler)
	cfg := newServer("", c.Handler, s)
	if cfg.ReadTimeout != s.HTTPReadTimeout || cfg.IdleTimeout != s.HTTPIdleTimeout {
		t.Fatalf("newServer timeouts = %v, %v", cfg.ReadTimeout, cfg.IdleTimeout)
	}
	ts.Config = cfg
	ts.Start()
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	tok := w.Tokens[matrix.ServiceAdmin]
	body := `{"bundle":{"kind":"` + strings.Repeat("x", 40) + `"},"accountMap":{}}`

	// Any other route: the connection is closed (no response, or a refusal of the unreadable body).
	code, out, took, err := trickle(t, addr, "/v1/teams", tok, body, time.Second)
	if err == nil && code == http.StatusCreated {
		t.Fatalf("a slow body was accepted: %d %s", code, out)
	}
	if took > 3*time.Second {
		t.Fatalf("slow body held the connection for %v with a 300ms read timeout", took)
	}

	// CreateImport reads for as long as 10 minutes, so the same pause is served and the body is read whole.
	code, out, _, err = trickle(t, addr, "/v1/imports", tok, body, time.Second)
	if err != nil {
		t.Fatalf("CreateImport slow body: %v", err)
	}
	if strings.Contains(out, "unreadable request body") {
		t.Fatalf("CreateImport cut the slow body: %d %s", code, out)
	}
}
