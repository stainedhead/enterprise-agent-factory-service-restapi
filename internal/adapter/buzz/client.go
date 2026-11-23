// Package buzz is the reconciler's client of a team's Buzz relay member administration (ORC-9). It talks only to
// the base URL a Resolver returns (the in-namespace Service) with an AFS-managed admin key, and never puts a
// credential, a public key list or a response body into an error or a log: a failed call reports the operation
// and the HTTP status only.
//
// The relay's admin API is an upstream dependency (OR-OQ-4). This client implements the surface AFS asks for:
// PUT /admin/members/{publicKey} with {"name": ...} adds a member (idempotent) and DELETE removes it (a missing
// member is not an error). Contract tests run against httptest.
package buzz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// Target is where a team's relay is and the admin key to call it with.
type Target struct {
	BaseURL  string
	AdminKey collab.Key
}

// Resolver finds a team's Target.
type Resolver interface {
	Target(ctx context.Context, teamID string) (Target, error)
}

// Error is a failed call: the operation and the status only (status 0 is a transport failure).
type Error struct {
	Op     string
	Status int
}

// Error implements error without any response content.
func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("buzz: %s: request failed", e.Op)
	}
	return fmt.Sprintf("buzz: %s: status %d", e.Op, e.Status)
}

// Client implements collab.BuzzRelay.
type Client struct {
	Resolver Resolver
	HTTP     *http.Client
}

var _ collab.BuzzRelay = (*Client)(nil)

// New returns a Client with a 15 second timeout.
func New(r Resolver) *Client {
	return &Client{Resolver: r, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) do(ctx context.Context, op, teamID, method, publicKey string, in any, okNotFound bool) error {
	t, err := c.Resolver.Target(ctx, teamID)
	if err != nil {
		return fmt.Errorf("buzz: %s: %w", op, err)
	}
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("buzz: %s: %w", op, err)
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.BaseURL, "/")+"/admin/members/"+url.PathEscape(publicKey), body)
	if err != nil {
		return &Error{Op: op}
	}
	req.Header.Set("Authorization", "Bearer "+t.AdminKey.Reveal())
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return &Error{Op: op}
	}
	defer func() { _ = resp.Body.Close() }()
	if okNotFound && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Op: op, Status: resp.StatusCode}
	}
	return nil
}

// AddMember implements collab.BuzzRelay.
func (c *Client) AddMember(ctx context.Context, teamID, name, publicKey string) error {
	if publicKey == "" {
		return errors.New("buzz: add member: empty public key")
	}
	return c.do(ctx, "add member", teamID, http.MethodPut, publicKey, map[string]string{"name": name}, false)
}

// RemoveMember implements collab.BuzzRelay; a member that is not there is already removed.
func (c *Client) RemoveMember(ctx context.Context, teamID, publicKey string) error {
	return c.do(ctx, "remove member", teamID, http.MethodDelete, publicKey, nil, true)
}
