package collab

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Link is one connected or formerly connected worker as the reconciler reads it.
type Link struct {
	TeamID, AgentID  string
	Connect          bool
	LinkState        domain.LinkState
	PaperclipAgentID string
	// KeyPresent is true when the worker's paperclip-agent-api-key secret has a value (RC-22): a connected
	// worker whose key has no value yet stays pending.
	KeyPresent bool
	// CollabAgentGone is true when the team's collaboration agent no longer exists.
	CollabAgentGone bool
	AgentGone       bool
	Mode            domain.RegistrationMode
}

// LinkSource reads the links and writes their observed state.
type LinkSource interface {
	Links(ctx context.Context) ([]Link, error)
	// SetState records the observed link state with the time and a sanitized error (empty to clear).
	SetState(ctx context.Context, teamID, agentID string, st domain.LinkState, lastErr string, at time.Time) error
}

// LinkSync keeps link_state true to the facts in manual registration (RC-22). Managed registration
// writes the same fields through the Paperclip client and is not part of this job (RC-25 is parked).
type LinkSync struct {
	Source LinkSource
	Events Events
	Now    func() time.Time
	Log    *slog.Logger

	mu   sync.Mutex
	last map[string]domain.LinkState // agent id to the state this process last saw
}

// Observed is the desired link state of a link.
func Observed(l Link) (st domain.LinkState, reason string) {
	switch {
	case !l.Connect:
		return domain.LinkDisabled, "disconnected"
	case l.AgentGone:
		return domain.LinkDisabled, "agent_deleted"
	case l.CollabAgentGone:
		return domain.LinkDisabled, "collaboration_agent_deleted"
	case l.PaperclipAgentID != "" && l.KeyPresent:
		return domain.LinkLinked, ""
	}
	return domain.LinkPending, ""
}

// Sync runs once. It writes a link state only when it differs, and emits agent.collaboration_linked when
// a worker becomes linked and agent.collaboration_unlinked when one that this process saw linked is not.
func (s *LinkSync) Sync(ctx context.Context) error {
	links, err := s.Source.Links(ctx)
	if err != nil {
		return fmt.Errorf("collab: read links: %w", err)
	}
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string]domain.LinkState{}
	}
	s.mu.Unlock()
	var failed int
	for _, l := range links {
		if l.Mode == domain.RegistrationManaged {
			continue // the managed registration job owns these rows
		}
		if err := s.one(ctx, l); err != nil {
			failed++
			s.log().Warn("link sync failed", "team", l.TeamID, "agent", l.AgentID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d links failed", failed)
	}
	return nil
}

func (s *LinkSync) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *LinkSync) one(ctx context.Context, l Link) error {
	want, reason := Observed(l)
	s.mu.Lock()
	prev, seen := s.last[l.AgentID]
	s.mu.Unlock()
	if l.LinkState != want {
		if err := s.Source.SetState(ctx, l.TeamID, l.AgentID, want, "", s.Now()); err != nil {
			return err
		}
	}
	wasLinked := seen && prev == domain.LinkLinked || !seen && l.LinkState == domain.LinkLinked
	data := map[string]any{"agentId": l.AgentID, "linkState": string(want), "registrationMode": string(modeOr(l.Mode))}
	if l.PaperclipAgentID != "" {
		data["paperclipAgentId"] = l.PaperclipAgentID
	}
	var err error
	switch {
	case want == domain.LinkLinked && !wasLinked:
		err = s.Events.Publish(ctx, domain.EventAgentCollaborationLinked, l.TeamID, "agents", l.AgentID, l.AgentID, data)
	case want != domain.LinkLinked && wasLinked:
		if reason == "" {
			reason = "failed"
		}
		data["reason"] = reason
		err = s.Events.Publish(ctx, domain.EventAgentCollaborationUnlinked, l.TeamID, "agents", l.AgentID, l.AgentID, data)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.last[l.AgentID] = want
	s.mu.Unlock()
	return nil
}

func modeOr(m domain.RegistrationMode) domain.RegistrationMode {
	if m == "" {
		return domain.RegistrationManual
	}
	return m
}
