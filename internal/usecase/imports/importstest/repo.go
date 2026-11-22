// Package importstest holds an in-memory imports.Repository for use case tests.
package importstest

import (
	"context"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo records what an import applied. Existing lists the slugs that already exist; FailSlug makes
// that team fail after it wrote something.
type Repo struct {
	Existing map[string]bool
	FailSlug string
	Applied  []domain.TeamBundle
	Catalog  [][]domain.Row
	Records  []imports.Record
}

// New returns an empty Repo.
func New() *Repo { return &Repo{Existing: map[string]bool{}} }

// ApplyCatalog implements imports.Repository.
func (r *Repo) ApplyCatalog(_ context.Context, _ port.Tx, s []domain.Row) ([]imports.Change, error) {
	r.Catalog = append(r.Catalog, s)
	return []imports.Change{{Kind: "external_systems", ID: "ext_1", Name: "x", Action: "created"}}, nil
}

// ApplyTeam implements imports.Repository.
func (r *Repo) ApplyTeam(_ context.Context, _ port.Tx, t domain.TeamBundle) (imports.TeamOutcome, error) {
	r.Applied = append(r.Applied, t)
	slug := t.Slug()
	if slug == r.FailSlug {
		return imports.TeamOutcome{}, domain.Validation(domain.FieldError{Field: "agents", Reason: "model is not enabled"})
	}
	if r.Existing[slug] {
		if len(t.Agents) == 0 {
			return imports.TeamOutcome{TeamID: "team_" + slug}, nil
		}
		return imports.TeamOutcome{TeamID: "team_" + slug, Changes: []imports.Change{{Kind: "agents", ID: "agt_1", Name: "coder", Action: "updated"}}}, nil
	}
	return imports.TeamOutcome{TeamID: "team_" + slug, Created: true,
		Changes: []imports.Change{{Kind: "teams", ID: "team_" + slug, Name: slug, Action: "created"}},
		Notes:   []string{fmt.Sprintf("secrets/%s: value must be put again", "token")}}, nil
}

// RecordImport implements imports.Repository.
func (r *Repo) RecordImport(_ context.Context, _ port.Tx, rec imports.Record) error {
	r.Records = append(r.Records, rec)
	return nil
}
