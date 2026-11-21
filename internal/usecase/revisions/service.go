// Package revisions implements ListRevisions and GetRevision (API contract §4.10): the history of a
// team's desired-state rows kept by the database's revision triggers. Reads only.
package revisions

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Revision is one config_revisions row. Snapshot is set by Get only.
type Revision struct {
	Revision     int64
	ResourceKind string
	ResourceID   string
	Operation    string
	Version      *int64
	ChangedBy    string
	ChangedAt    time.Time
	Snapshot     json.RawMessage
}

// Filter narrows ListRevisions.
type Filter struct{ ResourceKind, ResourceID string }

// Repository reads config_revisions.
type Repository interface {
	// List returns the team's revisions with id below before (0: newest), newest first, without
	// snapshots.
	List(ctx context.Context, tx port.Tx, teamID string, f Filter, before int64, limit int) ([]Revision, error)
	// Get returns one revision of the team with its snapshot; NotFound otherwise.
	Get(ctx context.Context, tx port.Tx, teamID string, rev int64) (Revision, error)
	// TeamExists reports whether the team row exists (deleted teams included) for the caller.
	TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error)
}

// Service is the revisions use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// List implements ListRevisions.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, f Filter, p acting.Page) ([]Revision, string, error) {
	p = p.Normalized()
	var before int64
	if p.Cursor != "" {
		n, err := strconv.ParseInt(p.Cursor, 10, 64)
		if err != nil {
			return nil, "", domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid token"})
		}
		before = n
	}
	var rows []Revision
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireTeam(ctx, tx, teamID); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, f, before, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r Revision) string { return strconv.FormatInt(r.Revision, 10) })
	return page, next, nil
}

func (s *Service) requireTeam(ctx context.Context, tx port.Tx, teamID string) error {
	ok, err := s.Repo.TeamExists(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NotFound("team")
	}
	return nil
}

// Get implements GetRevision.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string, rev int64) (Revision, error) {
	var out Revision
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireTeam(ctx, tx, teamID); err != nil {
			return err
		}
		var err error
		out, err = s.Repo.Get(ctx, tx, teamID, rev)
		return err
	})
	return out, err
}
