// Package logdest implements ListLogDestinations, CreateLogDestination, GetLogDestination,
// UpdateLogDestination and DeleteLogDestination (API contract §4.10, RC-18). A destination is
// desired state read by the reconciler's per-team Fluent Bit aggregator; it holds non-secret settings
// and a reference to the secret that carries the credential, never a credential. Contract §7 has no
// log destination event, so a change writes the row (the database notifies the reconciler) and an
// audit record (CD-7).
package logdest

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "log_destinations"

// Repository is the log_destinations store. Every method runs in the transaction it is given, so
// row-level security applies with the caller's session.
type Repository interface {
	// List returns the team's destinations with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.LogDestination, error)
	// Get returns one destination of the team (NotFound otherwise); forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.LogDestination, error)
	// SecretExists reports whether the team has a team-shared (not agent-scoped) secret with this id.
	SecretExists(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error)
	// Insert stores a destination; a duplicate name is a Conflict.
	Insert(ctx context.Context, tx port.Tx, d domain.LogDestination) error
	// Update stores the changed destination; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, d domain.LogDestination, expect domain.Version) (domain.LogDestination, error)
	// Delete removes the destination; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error
}

// Service is the log destination use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is a destination with the replayed response of an idempotent create.
type Result struct {
	Destination domain.LogDestination
	Replay      *port.StoredResponse
}

// canWrite is the use case's own check of logs:write (the route's middleware decides first; this keeps
// the use case safe when called from anywhere else). A denial is audited.
func (s *Service) canWrite(ctx context.Context, c acting.Caller, action, teamID string) error {
	switch d := authz.Authorize(c.Principal, authz.LogsWrite, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}); d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, authz.LogsWrite)
}

func auditOf(d domain.LogDestination) map[string]any {
	return map[string]any{"name": d.Name, "kind": d.Kind, "config": d.Config, "secretId": d.SecretID, "enabled": d.Enabled}
}

// List implements ListLogDestinations.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.LogDestination, string, error) {
	p = p.Normalized()
	var rows []domain.LogDestination
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(d domain.LogDestination) string { return d.ID })
	return page, next, nil
}

// Get implements GetLogDestination.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID, id string) (domain.LogDestination, error) {
	var d domain.LogDestination
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		d, err = s.Repo.Get(ctx, tx, teamID, id, false)
		return err
	})
	return d, err
}

func (s *Service) checkSecret(ctx context.Context, tx port.Tx, teamID, secretID string) error {
	if secretID == "" {
		return nil
	}
	ok, err := s.Repo.SecretExists(ctx, tx, teamID, secretID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Validation(domain.FieldError{Field: "secretId", Reason: "no such team secret (agent-scoped secrets cannot be used)"})
	}
	return nil
}

// Create implements CreateLogDestination.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewLogDestinationParams, idem *acting.Idem) (Result, error) {
	if err := s.canWrite(ctx, c, "CreateLogDestination", teamID); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := in.Validate(); err != nil {
			return err
		}
		if err := s.checkSecret(ctx, tx, teamID, in.SecretID); err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixLogDestination)
		if err != nil {
			return err
		}
		d, err := in.New(id.String(), teamID, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, d); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateLogDestination", kind, d.ID, teamID, nil, auditOf(d))); err != nil {
			return err
		}
		if d, err = s.Repo.Get(ctx, tx, teamID, d.ID, false); err != nil {
			return err
		}
		res.Destination = d
		return idem.Complete(ctx, s.Idem, tx, d)
	})
	return res, err
}

// Update implements UpdateLogDestination (If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, p domain.LogDestinationPatch, expect domain.Version) (domain.LogDestination, error) {
	if err := s.canWrite(ctx, c, "UpdateLogDestination", teamID); err != nil {
		return domain.LogDestination{}, err
	}
	var out domain.LogDestination
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(p, s.Now())
		if err != nil {
			return err
		}
		if next.SecretID != cur.SecretID {
			if err := s.checkSecret(ctx, tx, teamID, next.SecretID); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateLogDestination", kind, id, teamID, auditOf(cur), auditOf(out)))
	})
	return out, err
}

// Delete implements DeleteLogDestination (If-Match required). The row is removed in the transaction;
// the database notifies the reconciler, which drops the output from the team's aggregator. The
// returned destination is the removed one, in phase Deleting (CD-1).
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) (domain.LogDestination, error) {
	if err := s.canWrite(ctx, c, "DeleteLogDestination", teamID); err != nil {
		return domain.LogDestination{}, err
	}
	var out domain.LogDestination
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, teamID, id, expect); err != nil {
			return err
		}
		out = cur
		out.Status.Phase = domain.PhaseDeleting
		return s.Audit.Write(ctx, tx, c.Record("DeleteLogDestination", kind, id, teamID, auditOf(cur), nil))
	})
	return out, err
}
