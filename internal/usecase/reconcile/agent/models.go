package agent

import (
	"context"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// CondModelUnavailable is the condition that is True while an agent's or pool's default model is outside its
// effective set (MOD-6).
const CondModelUnavailable = "ModelUnavailable"

// Events puts an event on the outbox in the reconciler's own transaction (aggregate kind and id, the agent id
// when the event concerns one).
type Events interface {
	Publish(ctx context.Context, typ domain.EventType, teamID, kind, id, agentID string, data map[string]any) error
}

// ModelState is what the ModelUnavailable condition is computed from.
type ModelState struct {
	TeamID string
	// Kind is "agents" or "pools"; ID is the subject.
	Kind, ID string
	// Default is the default model id; Effective the models the subject may call (service, team, subject and
	// priced).
	Default   string
	Effective []string
	// Reason is why the default is refused (model_not_enabled_for_service|team|agent, model_not_priced); empty
	// when it is callable.
	Reason string
	// Prev is the ModelUnavailable status of the last reconcile, nil when there was none.
	Prev *bool
}

// Unavailable reports whether the default model is outside the effective set.
func (m ModelState) Unavailable() bool { return m.Reason != "" || len(m.Effective) == 0 }

// ModelCondition computes the condition: ModelUnavailable True with the model and the reason in the message,
// or False. An empty effective set is unavailable too, with the reason of the default or model_not_enabled_for_agent.
func ModelCondition(m ModelState) domain.Condition {
	if !m.Unavailable() {
		return domain.Condition{Type: CondModelUnavailable, Status: domain.ConditionFalse, Reason: "ModelAvailable"}
	}
	return domain.Condition{Type: CondModelUnavailable, Status: domain.ConditionTrue, Reason: modelReason(m),
		Message: "the default model " + m.Default + " is not callable: " + modelReason(m)}
}

func modelReason(m ModelState) string {
	if m.Reason != "" {
		return m.Reason
	}
	return domain.ReasonModelNotEnabledForAgent
}

// ReportModel publishes agent.model_unavailable once per transition to unavailable (a first sighting counts as
// a transition) and returns the condition. A failed publish is logged, not retried: the next reconcile sees the
// same condition as the previous one only after the status is written, so the event is attempted again until
// the pass succeeds.
func ReportModel(ctx context.Context, ev Events, log *slog.Logger, m ModelState) domain.Condition {
	cond := ModelCondition(m)
	if !m.Unavailable() || (m.Prev != nil && *m.Prev) || ev == nil {
		return cond
	}
	data := map[string]any{"teamId": m.TeamID, "defaultModelId": m.Default, "reason": modelReason(m), "effectiveModelIds": append([]string{}, m.Effective...)}
	agentID := ""
	if m.Kind == "pools" {
		data["poolId"] = m.ID
	} else {
		data["agentId"] = m.ID
		agentID = m.ID
	}
	if err := ev.Publish(ctx, domain.EventAgentModelUnavailable, m.TeamID, m.Kind, m.ID, agentID, data); err != nil && log != nil {
		log.Warn("model unavailable event not published", "team", m.TeamID, "subject", m.ID, "err", err)
	}
	return cond
}
