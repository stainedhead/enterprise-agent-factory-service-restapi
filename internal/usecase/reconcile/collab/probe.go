package collab

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Reasons of the PaperclipReady condition.
const (
	ReasonHealthy      = "Healthy"
	ReasonNotAnswering = "NotAnswering"
	ReasonOpenSignUp   = "OpenSignUp"
)

// Prober sets PaperclipReady for every managed team whose pod is ready (RC-24, COL-27): False with reason
// NotAnswering when Paperclip does not answer, False with reason OpenSignUp when anyone could create an
// account, True otherwise.
type Prober struct {
	Source Source
	Paper  Paperclip
	Status engine.StatusWriter
	Now    func() time.Time
	Log    *slog.Logger
}

// Run probes once.
func (p *Prober) Run(ctx context.Context) error {
	states, err := p.Source.States(ctx)
	if err != nil {
		return fmt.Errorf("collab: read states: %w", err)
	}
	var failed int
	for _, s := range states {
		if s.Mode != domain.RegistrationManaged || !s.PodReady {
			continue
		}
		if err := p.one(ctx, s); err != nil {
			failed++
			if p.Log != nil {
				p.Log.Warn("paperclip probe failed", "team", s.TeamID, "err", err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d probes failed", failed)
	}
	return nil
}

func (p *Prober) one(ctx context.Context, s TeamState) error {
	st, perr := p.Paper.Status(ctx, s.TeamID)
	ok, reason, msg := true, ReasonHealthy, ""
	switch {
	case perr != nil:
		ok, reason = false, ReasonNotAnswering
	case st.SignUpOpen:
		ok, reason, msg = false, ReasonOpenSignUp, "Paperclip sign-up is open: anyone could create an account (COL-27)"
	}
	now := p.Now()
	return p.Status.Apply(ctx, engine.Report{Ref: engine.NewRef(KindTeamCollaboration, s.TeamID, s.TeamID), Phase: phaseOf(s),
		Conditions: []domain.Condition{condition(ConditionPaperclipReady, ok, reason, msg, now)}, ReconciledAt: now})
}
