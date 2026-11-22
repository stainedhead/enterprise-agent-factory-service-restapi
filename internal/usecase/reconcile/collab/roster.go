package collab

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	dc "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// GuardInterval is how often the roster guard and the scan run (RC-26).
const GuardInterval = 10 * time.Minute

// RosterAgent is an agent in Paperclip's roster.
type RosterAgent struct {
	ID, Name         string
	Orchestrator     bool
	LocalAdapter     bool
	LinkedAFSAgentID string
}

// Item is an issue or comment of Paperclip with its text. The text exists only while it is scanned.
type Item struct {
	ID, AuthorAgentID string
	UpdatedAt         time.Time
	Text              string
}

// Paperclip is the read side of Paperclip's API the guard uses (managed mode only). The adapter that talks
// to a real Paperclip is a recorded upstream dependency (AFS_PAPERCLIP_BOOTSTRAP image).
type Paperclip interface {
	Roster(ctx context.Context, teamID string) ([]RosterAgent, error)
	// Changed returns the issues and comments created or changed after since, oldest first.
	Changed(ctx context.Context, teamID string, since time.Time) ([]Item, error)
}

// Unmanaged lists the roster agents that break the managed-mode rule, by reason: not linked to an AFS
// agent, or on a local adapter (other than the orchestrator).
func Unmanaged(roster []RosterAgent) map[string]string {
	out := map[string]string{}
	for _, a := range roster {
		switch {
		case a.Orchestrator:
		case a.LocalAdapter:
			out[a.Name] = "local_adapter"
		case a.LinkedAFSAgentID == "":
			out[a.Name] = "not_linked"
		}
	}
	return out
}

// SanitizeNames joins names for a condition message: only safe characters, cut to 500 characters.
func SanitizeNames(names []string) string {
	clean := make([]string, 0, len(names))
	for _, n := range names {
		clean = append(clean, strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '.' || r == '_' || r == '-' {
				return r
			}
			return '?'
		}, n))
	}
	s := strings.Join(clean, ", ")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// Guard is the roster guard and content scan of a team in managed mode (RC-26).
type Guard struct {
	Source Source
	Paper  Paperclip
	Status engine.StatusWriter
	Events Events
	Now    func() time.Time
	Log    *slog.Logger

	mu     sync.Mutex
	known  map[string]map[string]bool // team to names already announced
	cursor map[string]time.Time
}

// Run checks every managed team once.
func (g *Guard) Run(ctx context.Context) error {
	states, err := g.Source.States(ctx)
	if err != nil {
		return fmt.Errorf("collab: read states: %w", err)
	}
	var failed int
	for _, s := range states {
		if s.Mode != domain.RegistrationManaged || !s.PodReady {
			continue
		}
		if err := g.team(ctx, s); err != nil {
			failed++
			g.log().Warn("collaboration guard failed", "team", s.TeamID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d teams failed", failed)
	}
	return nil
}

func (g *Guard) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Guard) team(ctx context.Context, s TeamState) error {
	roster, err := g.Paper.Roster(ctx, s.TeamID)
	if err != nil {
		return err
	}
	bad := Unmanaged(roster)
	g.mu.Lock()
	if g.known == nil {
		g.known, g.cursor = map[string]map[string]bool{}, map[string]time.Time{}
	}
	prev, seeded := g.known[s.TeamID]
	if !seeded {
		prev = map[string]bool{}
		for _, n := range s.Unmanaged { // rebuilt from the stored condition after a leader change
			prev[n] = true
		}
	}
	g.mu.Unlock()
	names := make([]string, 0, len(bad))
	for n := range bad {
		names = append(names, n)
	}
	sort.Strings(names)
	cur := map[string]bool{}
	for _, n := range names {
		cur[n] = true
		if prev[n] {
			continue
		}
		ev := map[string]any{"teamId": s.TeamID, "agentId": s.AgentID, "paperclipAgentName": n, "reason": bad[n]}
		for _, a := range roster {
			if a.Name == n {
				ev["paperclipAgentId"] = a.ID
			}
		}
		if err := g.Events.Publish(ctx, domain.EventCollaborationUnmanagedAgentsFound, s.TeamID, string(KindTeamCollaboration), s.TeamID, s.AgentID, ev); err != nil {
			return err
		}
	}
	st, msg := domain.ConditionFalse, ""
	if len(names) > 0 {
		st, msg = domain.ConditionTrue, SanitizeNames(names)
	}
	now := g.Now()
	if err := g.Status.Apply(ctx, engine.Report{Ref: engine.NewRef(KindTeamCollaboration, s.TeamID, s.TeamID), Phase: phaseOf(s),
		Conditions: []domain.Condition{{Type: ConditionUnmanaged, Status: st, Reason: "RosterChecked", Message: msg, LastTransitionTime: now.UTC()}}, ReconciledAt: now}); err != nil {
		return err
	}
	g.mu.Lock()
	g.known[s.TeamID] = cur
	g.mu.Unlock()
	return g.scan(ctx, s)
}

func phaseOf(s TeamState) domain.Phase {
	if ComputeEndpoint(s).Ready {
		return domain.PhaseReady
	}
	return domain.PhasePending
}

// scan reads the issues and comments changed since the cursor and emits collaboration.secret_detected for
// each pattern found, never the text. Paperclip content is not copied anywhere.
func (g *Guard) scan(ctx context.Context, s TeamState) error {
	g.mu.Lock()
	since, ok := g.cursor[s.TeamID]
	g.mu.Unlock()
	if !ok {
		since = g.Now().Add(-24 * time.Hour)
	}
	items, err := g.Paper.Changed(ctx, s.TeamID, since)
	if err != nil {
		return err
	}
	last := since
	for _, it := range items {
		for _, m := range dc.Scan([]byte(it.Text)) {
			err := g.Events.Publish(ctx, domain.EventCollaborationSecretDetected, s.TeamID, string(KindTeamCollaboration), s.TeamID, s.AgentID,
				map[string]any{"teamId": s.TeamID, "source": "reconciler", "pattern": m.Pattern, "action": "detected", "count": m.Count,
					"paperclipItemId": it.ID, "authorAgentId": it.AuthorAgentID})
			if err != nil {
				return err
			}
		}
		if it.UpdatedAt.After(last) {
			last = it.UpdatedAt
		}
	}
	g.mu.Lock()
	g.cursor[s.TeamID] = last
	g.mu.Unlock()
	return nil
}
