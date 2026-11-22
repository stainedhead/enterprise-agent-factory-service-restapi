// Package paperclip is the HTTP client of a team's Paperclip API for the reconciler (RC-25, RC-26,
// RC-29, COL-12, COL-13, COL-25, COL-32, COL-33). It authenticates with the team's board key, talks only to
// the base URL a Resolver returns (the in-namespace Service), and never puts a credential or a response
// body into an error or a log: a failed call reports the operation and the HTTP status only.
//
// The API surface used here is the pinned Paperclip version's documented one (the template pins the image
// digest, PC-R5); the contract tests run it against papertest.Server, which implements the same surface.
package paperclip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// Target is where a team's Paperclip is and the credential to call it with.
type Target struct {
	BaseURL  string
	BoardKey collab.Key
}

// Resolver finds a team's Target. It fails when the team has no board key yet.
type Resolver interface {
	Target(ctx context.Context, teamID string) (Target, error)
}

// Error is a failed call: the operation and the status only (status 0 is a transport failure).
type Error struct {
	Op     string
	Status int
	Err    error
}

// Error implements error without any response content.
func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("paperclip: %s: request failed", e.Op)
	}
	return fmt.Sprintf("paperclip: %s: status %d", e.Op, e.Status)
}

// Unwrap returns the transport error, if any.
func (e *Error) Unwrap() error { return e.Err }

// IsNotFound reports whether err is a 404 from Paperclip.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

const (
	maxBody   = 4 << 20
	pageLimit = 100
	maxPages  = 50
)

// Client implements collab.Paperclip.
type Client struct {
	Resolver Resolver
	HTTP     *http.Client
}

var _ collab.Paperclip = (*Client)(nil)

// New returns a Client with a 15 second timeout.
func New(r Resolver) *Client {
	return &Client{Resolver: r, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) do(ctx context.Context, op, teamID, method, path string, query url.Values, in, out any) error {
	t, err := c.Resolver.Target(ctx, teamID)
	if err != nil {
		return fmt.Errorf("paperclip: %s: %w", op, err)
	}
	u := strings.TrimRight(t.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("paperclip: %s: %w", op, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return &Error{Op: op}
	}
	req.Header.Set("Authorization", "Bearer "+t.BoardKey.Reveal())
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The error of net/http names the URL and nothing secret, but keep only the cause class.
		return &Error{Op: op, Err: errors.New("transport error")}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Op: op, Status: resp.StatusCode}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Op: op, Err: errors.New("unreadable response")}
	}
	return nil
}

// Status implements collab.Paperclip: GET /api/health.
func (c *Client) Status(ctx context.Context, teamID string) (collab.Status, error) {
	var out struct {
		Status string `json:"status"`
		SignUp string `json:"signUp"`
	}
	if err := c.do(ctx, "health", teamID, http.MethodGet, "/api/health", nil, nil, &out); err != nil {
		return collab.Status{}, err
	}
	if out.Status != "ok" {
		return collab.Status{}, &Error{Op: "health", Status: http.StatusServiceUnavailable}
	}
	return collab.Status{SignUpOpen: out.SignUp == "open"}, nil
}

type rosterWire struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	AdapterType string            `json:"adapterType"`
	Role        string            `json:"role"`
	Status      string            `json:"status"`
	Metadata    map[string]string `json:"metadata"`
}

// Roster implements collab.Paperclip: GET /api/agents, following nextCursor.
func (c *Client) Roster(ctx context.Context, teamID string) ([]collab.RosterAgent, error) {
	var out []collab.RosterAgent
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"limit": {fmt.Sprint(pageLimit)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Items      []rosterWire `json:"items"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := c.do(ctx, "list-agents", teamID, http.MethodGet, "/api/agents", q, nil, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Items {
			if a.Status == "revoked" {
				continue
			}
			out = append(out, collab.RosterAgent{ID: a.ID, Name: a.Name, Orchestrator: a.Role == "orchestrator",
				LocalAdapter: strings.HasSuffix(a.AdapterType, "_local"), Paused: a.Status == "paused", LinkedAFSAgentID: a.Metadata["afsAgentId"]})
		}
		if resp.NextCursor == "" {
			return out, nil
		}
		cursor = resp.NextCursor
	}
	return nil, &Error{Op: "list-agents", Err: errors.New("too many pages")}
}

// Changed implements collab.Paperclip: GET /api/activity?since=, issues and comments, oldest first.
func (c *Client) Changed(ctx context.Context, teamID string, since time.Time) ([]collab.Item, error) {
	var out []collab.Item
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}, "limit": {fmt.Sprint(pageLimit)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Items []struct {
				ID            string    `json:"id"`
				Kind          string    `json:"kind"`
				AuthorAgentID string    `json:"authorAgentId"`
				UpdatedAt     time.Time `json:"updatedAt"`
				Text          string    `json:"text"`
			} `json:"items"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.do(ctx, "list-activity", teamID, http.MethodGet, "/api/activity", q, nil, &resp); err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			if it.Kind != "issue" && it.Kind != "comment" {
				continue
			}
			out = append(out, collab.Item{ID: it.ID, AuthorAgentID: it.AuthorAgentID, UpdatedAt: it.UpdatedAt, Text: it.Text})
		}
		if resp.NextCursor == "" {
			return out, nil
		}
		cursor = resp.NextCursor
	}
	return nil, &Error{Op: "list-activity", Err: errors.New("too many pages")}
}

// CreateAgent implements collab.Paperclip: POST /api/agents.
func (c *Client) CreateAgent(ctx context.Context, teamID string, a collab.NewAgent) (collab.CreatedAgent, error) {
	in := map[string]any{"name": a.Name, "adapterType": "remote", "persona": a.Persona, "metadata": map[string]string{"afsAgentId": a.AFSAgentID}}
	var out struct {
		ID     string `json:"id"`
		APIKey string `json:"apiKey"`
	}
	if err := c.do(ctx, "create-agent", teamID, http.MethodPost, "/api/agents", nil, in, &out); err != nil {
		return collab.CreatedAgent{}, err
	}
	if out.ID == "" || out.APIKey == "" {
		return collab.CreatedAgent{}, &Error{Op: "create-agent", Err: errors.New("response without id or key")}
	}
	return collab.CreatedAgent{ID: out.ID, Key: collab.Key(out.APIKey)}, nil
}

// SetAgentPaused implements collab.Paperclip: POST /api/agents/{id}/pause or /resume.
func (c *Client) SetAgentPaused(ctx context.Context, teamID, agentID string, paused bool) error {
	verb := "resume"
	if paused {
		verb = "pause"
	}
	return c.do(ctx, verb+"-agent", teamID, http.MethodPost, "/api/agents/"+url.PathEscape(agentID)+"/"+verb, nil, nil, nil)
}

// RevokeAgent implements collab.Paperclip: POST /api/agents/{id}/revoke.
func (c *Client) RevokeAgent(ctx context.Context, teamID, agentID string) error {
	err := c.do(ctx, "revoke-agent", teamID, http.MethodPost, "/api/agents/"+url.PathEscape(agentID)+"/revoke", nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Invites implements collab.Paperclip: GET /api/invites.
func (c *Client) Invites(ctx context.Context, teamID string) ([]collab.InviteInfo, error) {
	var resp struct {
		Items []struct {
			ID         string     `json:"id"`
			Email      string     `json:"email"`
			Role       string     `json:"role"`
			AcceptedAt *time.Time `json:"acceptedAt"`
		} `json:"items"`
	}
	if err := c.do(ctx, "list-invites", teamID, http.MethodGet, "/api/invites", nil, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]collab.InviteInfo, 0, len(resp.Items))
	for _, i := range resp.Items {
		out = append(out, collab.InviteInfo{ID: i.ID, Email: i.Email, Role: collab.InviteRole(i.Role), Accepted: i.AcceptedAt != nil})
	}
	return out, nil
}

// CreateInvite implements collab.Paperclip: POST /api/invites.
func (c *Client) CreateInvite(ctx context.Context, teamID, email string, role collab.InviteRole) (collab.Invite, error) {
	var out struct {
		ID        string `json:"id"`
		InviteURL string `json:"inviteUrl"`
	}
	if err := c.do(ctx, "create-invite", teamID, http.MethodPost, "/api/invites", nil, map[string]string{"email": email, "role": string(role)}, &out); err != nil {
		return collab.Invite{}, err
	}
	if out.ID == "" || out.InviteURL == "" {
		return collab.Invite{}, &Error{Op: "create-invite", Err: errors.New("response without id or url")}
	}
	return collab.Invite{ID: out.ID, URL: collab.Key(out.InviteURL)}, nil
}

// RevokeInvite implements collab.Paperclip: DELETE /api/invites/{id}.
func (c *Client) RevokeInvite(ctx context.Context, teamID, inviteID string) error {
	err := c.do(ctx, "revoke-invite", teamID, http.MethodDelete, "/api/invites/"+url.PathEscape(inviteID), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeactivateMember implements collab.Paperclip: POST /api/members/deactivate.
func (c *Client) DeactivateMember(ctx context.Context, teamID, email string) error {
	err := c.do(ctx, "deactivate-member", teamID, http.MethodPost, "/api/members/deactivate", nil, map[string]string{"email": email}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
