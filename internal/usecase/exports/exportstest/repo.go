// Package exportstest holds an in-memory exports.Repository for use case tests.
package exportstest

import (
	"context"
	"slices"
	"sort"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo is an in-memory repository.
type Repo struct {
	Teams   map[string]bool
	Exports map[string]domain.Export
}

// New returns an empty Repo.
func New() *Repo { return &Repo{Teams: map[string]bool{}, Exports: map[string]domain.Export{}} }

// MissingTeams implements exports.Repository.
func (r *Repo) MissingTeams(_ context.Context, _ port.Tx, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		if !r.Teams[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// Insert implements exports.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, e domain.Export) error {
	r.Exports[e.ID] = e
	return nil
}

// Get implements exports.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, id string) (domain.Export, error) {
	e, ok := r.Exports[id]
	if !ok {
		return domain.Export{}, domain.NotFound("export")
	}
	return e, nil
}

// List implements exports.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, before string, limit int) ([]domain.Export, error) {
	var out []domain.Export
	for _, e := range r.Exports {
		if before == "" || e.ID < before {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return slices.Clip(out[:min(limit, len(out))]), nil
}
