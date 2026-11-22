// Package imports implements CreateImport (RA-12): it applies an export bundle sent in the request
// body, synchronously, one transaction per team, as ordinary desired-state writes (row, audit and
// outbox per resource). afs-api never reads or writes S3 for an import (RA-US-14.5).
package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Change is one resource an import created or updated.
type Change struct {
	// Kind is the table of the resource, for example agents.
	Kind string
	ID   string
	// Name identifies the resource for people: the agent name, the tag key, ...
	Name string
	// Action is created or updated.
	Action string
}

// TeamOutcome is what applying one team did.
type TeamOutcome struct {
	TeamID  string
	Created bool
	Changes []Change
	// Notes are findings that are not writes: secrets that need a value, members skipped.
	Notes []string
}

// Record is the export_bundles row of an import.
type Record struct {
	ID            string
	SchemaVersion int
	SHA256        string
	DryRun        bool
	Failed        bool
	Summary       map[string]any
}

// Repository is the database side of an import. Every method runs in the transaction it is given.
type Repository interface {
	// ApplyCatalog creates the service level external systems that do not exist yet.
	ApplyCatalog(ctx context.Context, tx port.Tx, systems []domain.Row) ([]Change, error)
	// ApplyTeam creates or updates one team and everything in its bundle.
	ApplyTeam(ctx context.Context, tx port.Tx, t domain.TeamBundle) (TeamOutcome, error)
	// RecordImport stores the import row.
	RecordImport(ctx context.Context, tx port.Tx, r Record) error
}

// Service is the import use case. Access (admin:export, service admins only) is enforced by the route.
type Service struct {
	acting.Deps
	Repo Repository
}

// Input is a CreateImport request.
type Input struct {
	Bundle     json.RawMessage
	AccountMap map[string]string
	// DryRun computes the report and writes no desired state.
	DryRun bool
}

// TeamResult is the report line of one team.
type TeamResult struct {
	Slug    string
	Result  string
	Changes []string
	Error   *string
}

// Report is the response of CreateImport.
type Report struct {
	ID     string
	DryRun bool
	Teams  []TeamResult
}

// Result is a Report or the replay of an earlier response.
type Result struct {
	Report Report
	Replay *port.StoredResponse
}

// Team results.
const (
	ResultCreated = "created"
	ResultUpdated = "updated"
	ResultSkipped = "skipped"
	ResultFailed  = "failed"
)

var errRollback = errors.New("imports: dry run rolls back")

// Run implements CreateImport. A checksum or schema version that does not verify, or an invalid account
// map, is a ValidationError and nothing is written. A team that cannot be applied is reported failed with
// nothing written for it; the other teams are still applied.
func (s *Service) Run(ctx context.Context, c acting.Caller, in Input, idem *acting.Idem) (Result, error) {
	bundle, err := domain.ParseBundle(in.Bundle)
	if err != nil {
		return Result{}, err
	}
	if err := domain.ValidateAccountMap(in.AccountMap); err != nil {
		return Result{}, err
	}
	if in.DryRun {
		idem = nil // nothing is written, so a replay is harmless
	}
	if idem != nil {
		var replay *port.StoredResponse
		if err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
			var err error
			replay, err = idem.Begin(ctx, s.Idem, tx)
			return err
		}); err != nil || replay != nil {
			return Result{Replay: replay}, err
		}
	}
	id, err := s.IDs.NewID(domain.PrefixExport)
	if err != nil {
		return Result{}, err
	}
	rep := Report{ID: id.String(), DryRun: in.DryRun, Teams: []TeamResult{}}
	if err := s.applyCatalog(ctx, c, bundle.ExternalSystems, in.DryRun); err != nil {
		return Result{}, err
	}
	for _, t := range bundle.Teams {
		rep.Teams = append(rep.Teams, s.applyTeam(ctx, c, t, in))
	}
	if err := s.record(ctx, c, bundle, rep, idem); err != nil {
		return Result{}, err
	}
	return Result{Report: rep}, nil
}

func (s *Service) applyCatalog(ctx context.Context, c acting.Caller, systems []domain.Row, dry bool) error {
	if len(systems) == 0 {
		return nil
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		changes, err := s.Repo.ApplyCatalog(ctx, tx, systems)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			if err := s.Audit.Write(ctx, tx, c.Record("ImportResource", ch.Kind, ch.ID, "", nil, map[string]any{"name": ch.Name, "action": ch.Action})); err != nil {
				return err
			}
		}
		if dry {
			return errRollback
		}
		return nil
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}

func (s *Service) applyTeam(ctx context.Context, c acting.Caller, t domain.TeamBundle, in Input) TeamResult {
	res := TeamResult{Slug: t.Slug(), Changes: []string{}}
	fail := func(err error) TeamResult {
		msg := failureText(err)
		res.Result, res.Error, res.Changes = ResultFailed, &msg, []string{}
		return res
	}
	mapped, err := domain.RewriteAccounts(t, in.AccountMap)
	if err != nil {
		return fail(err)
	}
	var out TeamOutcome
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		out = TeamOutcome{}
		o, err := s.Repo.ApplyTeam(ctx, tx, mapped)
		if err != nil {
			return err
		}
		out = o
		for _, ch := range o.Changes {
			if err := s.Audit.Write(ctx, tx, c.Record("ImportResource", ch.Kind, ch.ID, o.TeamID, nil,
				map[string]any{"name": ch.Name, "action": ch.Action})); err != nil {
				return err
			}
		}
		if in.DryRun {
			return errRollback
		}
		if len(o.Changes) == 0 {
			return nil
		}
		typ := domain.EventTeamUpdated
		if o.Created {
			typ = domain.EventTeamCreated
		}
		return s.Emit(ctx, tx, c, typ, o.TeamID, "team", o.TeamID, map[string]any{"import": true})
	})
	if err != nil && !errors.Is(err, errRollback) {
		return fail(err)
	}
	switch {
	case out.Created:
		res.Result = ResultCreated
	case len(out.Changes) > 0:
		res.Result = ResultUpdated
	default:
		res.Result = ResultSkipped
	}
	for _, ch := range out.Changes {
		res.Changes = append(res.Changes, fmt.Sprintf("%s/%s: %s", ch.Kind, ch.Name, ch.Action))
	}
	res.Changes = append(res.Changes, out.Notes...)
	return res
}

// failureText is the reason shown for a failed team: the text of an API error, never a raw database or
// driver message.
func failureText(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return "the team could not be written"
}

func (s *Service) record(ctx context.Context, c acting.Caller, b domain.Bundle, rep Report, idem *acting.Idem) error {
	counts := map[string]int{ResultCreated: 0, ResultUpdated: 0, ResultSkipped: 0, ResultFailed: 0}
	for _, t := range rep.Teams {
		counts[t.Result]++
	}
	summary := map[string]any{"teams": counts, "dryRun": rep.DryRun}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.Repo.RecordImport(ctx, tx, Record{ID: rep.ID, SchemaVersion: b.SchemaVersion, SHA256: b.SHA256, DryRun: rep.DryRun,
			Failed: counts[ResultFailed] > 0, Summary: summary}); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateImport", "export_bundles", rep.ID, "", nil, summary)); err != nil {
			return err
		}
		if !rep.DryRun {
			if err := s.Emit(ctx, tx, c, domain.EventImportApplied, "", "import", rep.ID,
				map[string]any{"importId": rep.ID, "created": counts[ResultCreated], "updated": counts[ResultUpdated],
					"skipped": counts[ResultSkipped], "failed": counts[ResultFailed]}); err != nil {
				return err
			}
		}
		return idem.Complete(ctx, s.Idem, tx, rep)
	})
}
