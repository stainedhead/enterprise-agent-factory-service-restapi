// Package papertest is a fake Paperclip API for contract tests: it implements the surface the client in
// package paperclip calls, with in-memory state, board-key authentication and request recording.
package papertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Agent is a roster entry.
type Agent struct {
	ID, Name, AdapterType, Role, Status string
	AFSAgentID                          string
	Key                                 string
}

// Activity is an issue or comment.
type Activity struct {
	ID, Kind, AuthorAgentID, Text string
	UpdatedAt                     time.Time
}

// Invite is an invite.
type Invite struct {
	ID, Email, Role, URL string
	Accepted             bool
}

// Server is the fake. Set the exported knobs before the calls they affect; they are guarded by the lock.
type Server struct {
	*httptest.Server
	BoardKey string

	mu       sync.Mutex
	agents   map[string]*Agent
	order    []string
	items    []Activity
	invites  map[string]*Invite
	inviteSq int
	agentSq  int
	signUp   string
	down     bool
	failNext map[string]int // "METHOD path-prefix" to status
	pageSize int
	Requests []string // "METHOD path" of every authenticated call
	members  map[string]bool
}

// New starts a fake whose calls need "Authorization: Bearer boardKey".
func New(boardKey string) *Server {
	s := &Server{BoardKey: boardKey, agents: map[string]*Agent{}, invites: map[string]*Invite{}, signUp: "closed", failNext: map[string]int{},
		pageSize: 100, members: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Lock-guarded setters and getters.

// SetSignUp sets "open" or "closed".
func (s *Server) SetSignUp(v string) { s.mu.Lock(); s.signUp = v; s.mu.Unlock() }

// SetDown makes the health endpoint answer 503.
func (s *Server) SetDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

// SetPageSize bounds a page (to test pagination).
func (s *Server) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// FailNext makes the next call matching "METHOD /path-prefix" answer status.
func (s *Server) FailNext(key string, status int) {
	s.mu.Lock()
	s.failNext[key] = status
	s.mu.Unlock()
}

// AddAgent seeds a roster entry (an orchestrator or an unmanaged agent).
func (s *Server) AddAgent(a Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == "" {
		s.agentSq++
		a.ID = fmt.Sprintf("pc_%d", s.agentSq)
	}
	if a.Status == "" {
		a.Status = "active"
	}
	s.agents[a.ID] = &a
	s.order = append(s.order, a.ID)
}

// RemoveAgent deletes a roster entry outright (someone removed it in Paperclip).
func (s *Server) RemoveAgent(id string) { s.mu.Lock(); delete(s.agents, id); s.mu.Unlock() }

// AgentByName returns a copy of the agent named name.
func (s *Server) AgentByName(name string) (Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.Name == name {
			return *a, true
		}
	}
	return Agent{}, false
}

// AgentByID returns a copy of the agent.
func (s *Server) AgentByID(id string) (Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		return *a, true
	}
	return Agent{}, false
}

// AddActivity seeds an issue or comment.
func (s *Server) AddActivity(a Activity) { s.mu.Lock(); s.items = append(s.items, a); s.mu.Unlock() }

// Invites returns copies of the invites.
func (s *Server) Invites() []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Invite
	for _, i := range s.invites {
		out = append(out, *i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// SeedInvite adds an invite.
func (s *Server) SeedInvite(i Invite) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i.ID == "" {
		s.inviteSq++
		i.ID = fmt.Sprintf("inv_%d", s.inviteSq)
	}
	s.invites[i.ID] = &i
}

// Deactivated reports whether email was deactivated.
func (s *Server) Deactivated(email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members[email]
}

// Calls returns the recorded "METHOD path" list.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Requests...)
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+s.BoardKey {
		write(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	s.Requests = append(s.Requests, r.Method+" "+r.URL.Path)
	for k, st := range s.failNext {
		m, p, _ := strings.Cut(k, " ")
		if m == r.Method && strings.HasPrefix(r.URL.Path, p) {
			delete(s.failNext, k)
			write(w, st, map[string]string{"error": "injected secret-in-error-body"})
			return
		}
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api/health":
		if s.down {
			write(w, http.StatusServiceUnavailable, nil)
			return
		}
		write(w, 200, map[string]string{"status": "ok", "signUp": s.signUp})
	case r.Method == http.MethodGet && p == "/api/agents":
		s.listAgents(w, r)
	case r.Method == http.MethodPost && p == "/api/agents":
		var in struct {
			Name, AdapterType, Persona string
			Metadata                   map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.agentSq++
		a := &Agent{ID: fmt.Sprintf("pc_%d", s.agentSq), Name: in.Name, AdapterType: in.AdapterType, Role: "agent", Status: "active",
			AFSAgentID: in.Metadata["afsAgentId"], Key: fmt.Sprintf("pck_secret_%d", s.agentSq)}
		s.agents[a.ID] = a
		s.order = append(s.order, a.ID)
		write(w, http.StatusCreated, map[string]string{"id": a.ID, "apiKey": a.Key})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/agents/"):
		s.agentAction(w, p)
	case r.Method == http.MethodGet && p == "/api/activity":
		s.listActivity(w, r)
	case r.Method == http.MethodGet && p == "/api/invites":
		var items []map[string]any
		ids := make([]string, 0, len(s.invites))
		for id := range s.invites {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			i := s.invites[id]
			m := map[string]any{"id": i.ID, "email": i.Email, "role": i.Role}
			if i.Accepted {
				m["acceptedAt"] = time.Now().UTC()
			}
			items = append(items, m)
		}
		write(w, 200, map[string]any{"items": items})
	case r.Method == http.MethodPost && p == "/api/invites":
		var in struct{ Email, Role string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.inviteSq++
		i := &Invite{ID: fmt.Sprintf("inv_%d", s.inviteSq), Email: in.Email, Role: in.Role, URL: fmt.Sprintf("https://paperclip.example/invite/tok%d", s.inviteSq)}
		s.invites[i.ID] = i
		write(w, http.StatusCreated, map[string]string{"id": i.ID, "inviteUrl": i.URL})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/invites/"):
		id := strings.TrimPrefix(p, "/api/invites/")
		if _, ok := s.invites[id]; !ok {
			write(w, http.StatusNotFound, nil)
			return
		}
		delete(s.invites, id)
		write(w, http.StatusNoContent, nil)
	case r.Method == http.MethodPost && p == "/api/members/deactivate":
		var in struct{ Email string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.members[in.Email] = true
		write(w, http.StatusNoContent, nil)
	default:
		write(w, http.StatusNotFound, nil)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	start := 0
	_, _ = fmt.Sscan(r.URL.Query().Get("cursor"), &start)
	var items []map[string]any
	next := ""
	for i := start; i < len(s.order); i++ {
		if len(items) == s.pageSize {
			next = fmt.Sprint(i)
			break
		}
		a, ok := s.agents[s.order[i]]
		if !ok {
			continue
		}
		items = append(items, map[string]any{"id": a.ID, "name": a.Name, "adapterType": a.AdapterType, "role": a.Role, "status": a.Status,
			"metadata": map[string]string{"afsAgentId": a.AFSAgentID}})
	}
	write(w, 200, map[string]any{"items": items, "nextCursor": next})
}

func (s *Server) agentAction(w http.ResponseWriter, p string) {
	rest := strings.TrimPrefix(p, "/api/agents/")
	id, verb, _ := strings.Cut(rest, "/")
	a, ok := s.agents[id]
	if !ok {
		write(w, http.StatusNotFound, nil)
		return
	}
	switch verb {
	case "pause":
		a.Status = "paused"
	case "resume":
		a.Status = "active"
	case "revoke":
		a.Status, a.Key = "revoked", ""
	default:
		write(w, http.StatusNotFound, nil)
		return
	}
	write(w, http.StatusNoContent, nil)
}

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
	start := 0
	_, _ = fmt.Sscan(r.URL.Query().Get("cursor"), &start)
	var match []Activity
	for _, a := range s.items {
		if a.UpdatedAt.After(since) {
			match = append(match, a)
		}
	}
	sort.SliceStable(match, func(i, j int) bool { return match[i].UpdatedAt.Before(match[j].UpdatedAt) })
	var items []map[string]any
	next := ""
	for i := start; i < len(match); i++ {
		if len(items) == s.pageSize {
			next = fmt.Sprint(i)
			break
		}
		a := match[i]
		items = append(items, map[string]any{"id": a.ID, "kind": a.Kind, "authorAgentId": a.AuthorAgentID, "updatedAt": a.UpdatedAt, "text": a.Text})
	}
	write(w, 200, map[string]any{"items": items, "nextCursor": next})
}
