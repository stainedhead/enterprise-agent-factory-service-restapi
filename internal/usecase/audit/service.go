// Package audit implements ListAuditEvents (team) and ListAdminAuditEvents (service), API contract
// §4.10 and §4.11. Reads only: the audit log is append-only.
package audit

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Event is one audit_log row.
type Event struct {
	ID           int64
	OccurredAt   time.Time
	ActorType    string
	ActorID      string
	Action       string
	Outcome      string
	ResourceKind string
	ResourceID   string
	TeamID       string
	RequestID    string
	SourceIP     string
	UserAgent    string
	WorkerPod    string
	Before       json.RawMessage
	After        json.RawMessage
}

// Filter narrows a listing; empty fields match everything.
type Filter struct {
	From, To     *time.Time
	ActorID      string
	Action       string
	Outcome      string
	ResourceKind string
	ResourceID   string
	// TeamID is forced from the path on the team route; optional on the service route.
	TeamID string
}

// Repository reads audit_log newest first.
type Repository interface {
	// List returns events with id below before (0 means from the newest), newest first.
	List(ctx context.Context, tx port.Tx, f Filter, before int64, limit int) ([]Event, error)
	// TeamExists reports whether the team row exists (deleted teams included) for the caller.
	TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error)
}

// Service is the audit use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

var validOutcomes = map[string]bool{"allowed": true, "denied": true, "failed": true}

func (f Filter) validate() error {
	if f.Outcome != "" && !validOutcomes[f.Outcome] {
		return domain.Validation(domain.FieldError{Field: "outcome", Reason: "must be allowed, denied or failed"})
	}
	if f.From != nil && f.To != nil && f.To.Before(*f.From) {
		return domain.Validation(domain.FieldError{Field: "to", Reason: "must not be before from"})
	}
	return nil
}

func (s *Service) list(ctx context.Context, c acting.Caller, f Filter, p acting.Page, checkTeam bool) ([]Event, string, error) {
	if err := f.validate(); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var before int64
	if p.Cursor != "" {
		n, err := strconv.ParseInt(p.Cursor, 10, 64)
		if err != nil {
			return nil, "", domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid token"})
		}
		before = n
	}
	var rows []Event
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if checkTeam {
			ok, err := s.Repo.TeamExists(ctx, tx, f.TeamID)
			if err != nil {
				return err
			}
			if !ok {
				return domain.NotFound("team")
			}
		}
		var err error
		rows, err = s.Repo.List(ctx, tx, f, before, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(e Event) string { return strconv.FormatInt(e.ID, 10) })
	return page, next, nil
}

// ListTeam implements ListAuditEvents; the team comes from the path.
func (s *Service) ListTeam(ctx context.Context, c acting.Caller, teamID string, f Filter, p acting.Page) ([]Event, string, error) {
	f.TeamID = teamID
	return s.list(ctx, c, f, p, true)
}

// ListService implements ListAdminAuditEvents (admin:audit, service admins only).
func (s *Service) ListService(ctx context.Context, c acting.Caller, f Filter, p acting.Page) ([]Event, string, error) {
	if !authz.Authorize(c.Principal, authz.AdminAudit, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil, "", s.Denied(ctx, c, "ListAdminAuditEvents", "", authz.AdminAudit)
	}
	return s.list(ctx, c, f, p, false)
}
