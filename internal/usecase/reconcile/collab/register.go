package collab

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// ConditionRegistrationHealthy is the condition that says managed registration is working (COL-12, PC-R5).
const ConditionRegistrationHealthy = "RegistrationHealthy"

// Tags of a secret the registrar writes: the same tags afs-api puts on a secret, so the team's secrets
// are found by the same filters.
const (
	tagTeamID  = "afs-team-id"
	tagAgentID = "afs-agent-id"
)

// ManagedLink is one worker agent's link in a team that uses managed registration.
type ManagedLink struct {
	TeamID, AgentID, AgentName, Persona string
	Connect, AgentGone, CollabGone      bool
	// Paused is true when the AFS agent is paused or its team is suspended: its Paperclip agent is paused too.
	Paused           bool
	LinkState        domain.LinkState
	PaperclipAgentID string
	// KeyARN is the ARN recorded for the agent's paperclip-agent-api-key secret ("" when none).
	KeyARN string
	// KeyPath is the Secrets Manager name the key goes to (afs.secret_path).
	KeyPath string
	// LastError is the last stored error of the link.
	LastError string
}

// ManagedTeam is a team in managed mode whose board key is recorded.
type ManagedTeam struct {
	TeamID, CollabAgentID string
	// PodReady is the collaboration agent's readiness: nothing is called while Paperclip is down.
	PodReady bool
	// Gone is true when the team's collaboration agent no longer exists: Paperclip is gone with its pod, so
	// only the key records and secrets of the links are cleaned.
	Gone  bool
	Links []ManagedLink
}

// ManagedUpdate is the observed state of a link written by the registrar.
type ManagedUpdate struct {
	State domain.LinkState
	// PaperclipAgentID is the roster id ("" clears it).
	PaperclipAgentID string
	// LastError is a sanitized message ("" clears it).
	LastError string
	At        time.Time
}

// ManagedSource reads managed teams and writes the observed link and the key records.
type ManagedSource interface {
	ManagedTeams(ctx context.Context) ([]ManagedTeam, error)
	// SetManaged writes link_state, paperclip_agent_id, last_synced_at and last_error.
	SetManaged(ctx context.Context, teamID, agentID string, u ManagedUpdate) error
	// RecordKey calls afs.record_collab_agent_key: the secret row and the PAPERCLIP_AGENT_API_KEY binding.
	RecordKey(ctx context.Context, teamID, agentID, arn string) error
	// RemoveKey calls afs.remove_collab_agent_key and reports whether a row was removed.
	RemoveKey(ctx context.Context, teamID, agentID string) (bool, error)
}

// KeyStore writes a worker's key to Secrets Manager. The value goes straight from Paperclip's response
// to this call and nowhere else.
type KeyStore interface {
	// Put creates the secret path with value and tags, or writes a new version when it exists, and
	// returns its ARN.
	Put(ctx context.Context, path string, value Key, tags map[string]string) (arn string, err error)
	// ScheduleDeletion schedules the secret for deletion (with the recovery window).
	ScheduleDeletion(ctx context.Context, arn string) error
}

// Registrar is managed registration (COL-12, RC-25): it keeps Paperclip's roster in step with the
// connected worker agents of every managed team.
type Registrar struct {
	Source ManagedSource
	Paper  Paperclip
	Keys   KeyStore
	Status engine.StatusWriter
	Events Events
	Now    func() time.Time
	Log    *slog.Logger
}

func (r *Registrar) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Run reconciles every managed team once.
func (r *Registrar) Run(ctx context.Context) error {
	teams, err := r.Source.ManagedTeams(ctx)
	if err != nil {
		return fmt.Errorf("collab: read managed teams: %w", err)
	}
	var failed int
	for _, t := range teams {
		if !t.PodReady {
			continue
		}
		if t.Gone {
			if err := r.gone(ctx, t); err != nil {
				failed++
				r.log().Warn("collaboration cleanup failed", "team", t.TeamID, "err", err)
			}
			continue
		}
		if err := r.team(ctx, t); err != nil {
			failed++
			r.log().Warn("managed registration failed", "team", t.TeamID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: managed registration failed for %d teams", failed)
	}
	return nil
}

// gone cleans the links of a team whose collaboration agent was deleted.
func (r *Registrar) gone(ctx context.Context, t ManagedTeam) error {
	var failed int
	for _, l := range t.Links {
		l.CollabGone = true
		if err := r.teardown(ctx, l); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d links failed", failed)
	}
	return nil
}

func (r *Registrar) team(ctx context.Context, t ManagedTeam) error {
	if len(t.Links) == 0 {
		return r.health(ctx, t, nil)
	}
	roster, err := r.Paper.Roster(ctx, t.TeamID)
	if err != nil {
		return r.healthDown(ctx, t, err)
	}
	byID := make(map[string]RosterAgent, len(roster))
	for _, a := range roster {
		byID[a.ID] = a
	}
	var failedIDs []string
	for _, l := range t.Links {
		if err := r.link(ctx, l, byID); err != nil {
			failedIDs = append(failedIDs, l.AgentID)
			r.log().Warn("collaboration link failed", "team", l.TeamID, "agent", l.AgentID, "err", err)
		}
	}
	return r.health(ctx, t, failedIDs)
}

func (r *Registrar) healthDown(ctx context.Context, t ManagedTeam, cause error) error {
	if err := r.apply(ctx, t, domain.ConditionFalse, "PaperclipUnreachable", "the Paperclip roster could not be read"); err != nil {
		return err
	}
	return cause
}

func (r *Registrar) health(ctx context.Context, t ManagedTeam, failed []string) error {
	if len(failed) == 0 {
		return r.apply(ctx, t, domain.ConditionTrue, "Registered", "")
	}
	sort.Strings(failed)
	msg := fmt.Sprintf("%d agents are not registered: %s", len(failed), strings.Join(failed, ", "))
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if err := r.apply(ctx, t, domain.ConditionFalse, "RegistrationFailed", msg); err != nil {
		return err
	}
	return fmt.Errorf("collab: %d links failed", len(failed))
}

func (r *Registrar) apply(ctx context.Context, t ManagedTeam, st domain.ConditionStatus, reason, msg string) error {
	now := r.Now()
	return r.Status.Apply(ctx, engine.Report{Ref: engine.NewRef(KindTeamCollaboration, t.TeamID, t.TeamID),
		Conditions: []domain.Condition{{Type: ConditionRegistrationHealthy, Status: st, Reason: reason, Message: msg, LastTransitionTime: now.UTC()}}, ReconciledAt: now})
}

func (r *Registrar) set(ctx context.Context, l ManagedLink, st domain.LinkState, id string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
	}
	if err := r.Source.SetManaged(ctx, l.TeamID, l.AgentID, ManagedUpdate{State: st, PaperclipAgentID: id, LastError: msg, At: r.Now()}); err != nil {
		return err
	}
	return r.announce(ctx, l, st)
}

// announce emits agent.collaboration_linked or _unlinked when the stored state moves into or out of linked.
func (r *Registrar) announce(ctx context.Context, l ManagedLink, now domain.LinkState) error {
	if l.LinkState == now {
		return nil
	}
	data := map[string]any{"agentId": l.AgentID, "linkState": string(now), "registrationMode": string(domain.RegistrationManaged)}
	switch {
	case now == domain.LinkLinked:
		if l.PaperclipAgentID != "" {
			data["paperclipAgentId"] = l.PaperclipAgentID
		}
		return r.Events.Publish(ctx, domain.EventAgentCollaborationLinked, l.TeamID, "agents", l.AgentID, l.AgentID, data)
	case l.LinkState == domain.LinkLinked:
		switch {
		case l.AgentGone:
			data["reason"] = "agent_deleted"
		case l.CollabGone:
			data["reason"] = "collaboration_agent_deleted"
		case !l.Connect:
			data["reason"] = "disconnected"
		default:
			data["reason"] = "failed"
		}
		return r.Events.Publish(ctx, domain.EventAgentCollaborationUnlinked, l.TeamID, "agents", l.AgentID, l.AgentID, data)
	}
	return nil
}

func (r *Registrar) link(ctx context.Context, l ManagedLink, roster map[string]RosterAgent) error {
	if !l.Connect || l.AgentGone || l.CollabGone {
		return r.teardown(ctx, l)
	}
	if l.PaperclipAgentID != "" {
		ra, ok := roster[l.PaperclipAgentID]
		if ok && l.KeyARN != "" {
			if ra.Paused != l.Paused {
				if err := r.Paper.SetAgentPaused(ctx, l.TeamID, l.PaperclipAgentID, l.Paused); err != nil {
					return r.fail(ctx, l, l.PaperclipAgentID, err)
				}
			}
			if l.LinkState == domain.LinkLinked {
				return nil
			}
			return r.set(ctx, l, domain.LinkLinked, l.PaperclipAgentID, nil)
		}
		// The Paperclip agent or the key is gone (removed by hand, or a registration that stopped half way):
		// clean up what is left and register again.
		if ok {
			if err := r.Paper.RevokeAgent(ctx, l.TeamID, l.PaperclipAgentID); err != nil {
				return r.fail(ctx, l, l.PaperclipAgentID, err)
			}
		}
		if err := r.dropKey(ctx, l); err != nil {
			return r.fail(ctx, l, "", err)
		}
		l.PaperclipAgentID, l.KeyARN = "", ""
	}
	return r.register(ctx, l)
}

func (r *Registrar) fail(ctx context.Context, l ManagedLink, id string, cause error) error {
	if err := r.set(ctx, l, domain.LinkFailed, id, cause); err != nil {
		return err
	}
	return cause
}

func (r *Registrar) register(ctx context.Context, l ManagedLink) error {
	created, err := r.Paper.CreateAgent(ctx, l.TeamID, NewAgent{Name: l.AgentName, AFSAgentID: l.AgentID, Persona: l.Persona})
	if err != nil {
		return r.fail(ctx, l, "", err)
	}
	// The id is recorded before the key is stored so a crash does not leave a Paperclip agent nobody knows.
	if err := r.Source.SetManaged(ctx, l.TeamID, l.AgentID, ManagedUpdate{State: domain.LinkPending, PaperclipAgentID: created.ID, At: r.Now()}); err != nil {
		_ = r.Paper.RevokeAgent(ctx, l.TeamID, created.ID)
		return err
	}
	arn, err := r.Keys.Put(ctx, l.KeyPath, created.Key, map[string]string{tagTeamID: l.TeamID, tagAgentID: l.AgentID})
	if err == nil {
		err = r.Source.RecordKey(ctx, l.TeamID, l.AgentID, arn)
		if err != nil {
			_ = r.Keys.ScheduleDeletion(ctx, arn)
		}
	}
	if err != nil {
		// The key is only shown once: without it the Paperclip agent is useless, so revoke it and start over.
		_ = r.Paper.RevokeAgent(ctx, l.TeamID, created.ID)
		l.PaperclipAgentID = ""
		return r.fail(ctx, l, "", err)
	}
	if l.Paused {
		if err := r.Paper.SetAgentPaused(ctx, l.TeamID, created.ID, true); err != nil {
			return r.fail(ctx, l, created.ID, err)
		}
	}
	l.PaperclipAgentID = created.ID
	return r.set(ctx, l, domain.LinkLinked, created.ID, nil)
}

// dropKey removes the key records and schedules the secret for deletion.
func (r *Registrar) dropKey(ctx context.Context, l ManagedLink) error {
	if _, err := r.Source.RemoveKey(ctx, l.TeamID, l.AgentID); err != nil {
		return err
	}
	if l.KeyARN != "" {
		return r.Keys.ScheduleDeletion(ctx, l.KeyARN)
	}
	return nil
}

// teardown handles a link that is disconnected or whose agent or collaboration agent is gone: revoke the
// key in Paperclip, remove the records and schedule the secret for deletion.
func (r *Registrar) teardown(ctx context.Context, l ManagedLink) error {
	if l.PaperclipAgentID == "" && l.KeyARN == "" {
		if l.LinkState == domain.LinkDisabled {
			return nil
		}
		return r.set(ctx, l, domain.LinkDisabled, "", nil)
	}
	if l.PaperclipAgentID != "" && !l.CollabGone { // the pod, and Paperclip with it, is gone
		if err := r.Paper.RevokeAgent(ctx, l.TeamID, l.PaperclipAgentID); err != nil {
			return r.keepError(ctx, l, err)
		}
	}
	if err := r.dropKey(ctx, l); err != nil {
		return r.keepError(ctx, l, err)
	}
	return r.set(ctx, l, domain.LinkDisabled, "", nil)
}

// keepError records the error and keeps the state and the Paperclip id, so the next pass retries.
func (r *Registrar) keepError(ctx context.Context, l ManagedLink, cause error) error {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if err := r.Source.SetManaged(ctx, l.TeamID, l.AgentID, ManagedUpdate{State: l.LinkState, PaperclipAgentID: l.PaperclipAgentID, LastError: msg, At: r.Now()}); err != nil {
		return err
	}
	return cause
}
