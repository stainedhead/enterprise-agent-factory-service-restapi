// Package restoretest holds an in-memory restore.Repository for use case tests.
package restoretest

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

// Repo is an in-memory repository. Refuse maps "<kind>/<id>" to a skip reason Apply returns.
type Repo struct {
	Lock      map[string]restore.TeamLock
	Revisions map[string]map[int64]bool
	Targets   domain.RestoreState
	Now       domain.RestoreState
	Refuse    map[string]string
	Applied   []domain.RestoreStep
	Gen       int64
	Locked    []string
}

// New returns a Repo with one team.
func New(team string) *Repo {
	return &Repo{Lock: map[string]restore.TeamLock{team: {State: domain.TeamStateActive, Version: 3, Generation: 5}},
		Revisions: map[string]map[int64]bool{team: {41: true}}, Targets: domain.RestoreState{}, Now: domain.RestoreState{},
		Refuse: map[string]string{}, Gen: 5}
}

// LockTeam implements restore.Repository.
func (r *Repo) LockTeam(_ context.Context, _ port.Tx, id string) (restore.TeamLock, bool, error) {
	l, ok := r.Lock[id]
	if ok {
		r.Locked = append(r.Locked, id)
	}
	return l, ok, nil
}

// HasRevision implements restore.Repository.
func (r *Repo) HasRevision(_ context.Context, _ port.Tx, team string, rev int64) (bool, error) {
	return r.Revisions[team][rev], nil
}

// Target implements restore.Repository.
func (r *Repo) Target(context.Context, port.Tx, string, int64) (domain.RestoreState, error) {
	return r.Targets, nil
}

// Current implements restore.Repository.
func (r *Repo) Current(context.Context, port.Tx, string) (domain.RestoreState, error) {
	return r.Now, nil
}

// Apply implements restore.Repository.
func (r *Repo) Apply(_ context.Context, _ port.Tx, _ string, s domain.RestoreStep) (string, error) {
	if why := r.Refuse[s.Change.ResourceKind+"/"+s.Change.ResourceID]; why != "" {
		return why, nil
	}
	r.Applied = append(r.Applied, s)
	return "", nil
}

// BumpGeneration implements restore.Repository.
func (r *Repo) BumpGeneration(context.Context, port.Tx, string) (int64, error) {
	r.Gen++
	return r.Gen, nil
}
