// Package mcpaccesstest is an in-memory mcpaccess.Store for use case tests.
package mcpaccesstest

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Store keeps the access by subject id. Every server the team has on is effective for an access all subject; a
// selected subject reaches its selected servers.
type Store struct {
	On      map[string][]string // team id -> switched-on server ids
	Default domain.McpAgentMode
	Access  map[string]domain.AgentMcp
	// Fail makes every call fail; FailPut and FailDefault only Put and DefaultMode.
	Fail, FailPut, FailDefault error
	// Puts counts the writes.
	Puts int
}

// New returns an empty store whose default mode is search.
func New() *Store {
	return &Store{On: map[string][]string{}, Default: domain.McpAgentModeSearch, Access: map[string]domain.AgentMcp{}}
}

var _ mcpaccess.Store = (*Store)(nil)

// TeamServers implements mcpaccess.Store.
func (s *Store) TeamServers(_ context.Context, _ port.Tx, teamID string) ([]string, error) {
	return s.On[teamID], s.Fail
}

// DefaultMode implements mcpaccess.Store.
func (s *Store) DefaultMode(context.Context, port.Tx) (domain.McpAgentMode, error) {
	return s.Default, errors.Join(s.Fail, s.FailDefault)
}

// Stored implements mcpaccess.Store; a subject never written reads as access all.
func (s *Store) Stored(_ context.Context, _ port.Tx, teamID, _ string, ids []string) (map[string]mcpaccess.Stored, error) {
	if s.Fail != nil {
		return nil, s.Fail
	}
	out := map[string]mcpaccess.Stored{}
	for _, id := range ids {
		a, ok := s.Access[id]
		if !ok {
			a = domain.AgentMcp{Access: "all"}
		}
		eff := a.ServerIDs
		if a.Access == "all" {
			eff = s.On[teamID]
		}
		out[id] = mcpaccess.Stored{Access: a, Effective: eff}
	}
	return out, nil
}

// Put implements mcpaccess.Store.
func (s *Store) Put(_ context.Context, _ port.Tx, _, _, id string, m domain.AgentMcp, blockIDs []string) error {
	if err := errors.Join(s.Fail, s.FailPut); err != nil {
		return err
	}
	if len(blockIDs) != len(m.Blocks) {
		return errors.New("one id per block")
	}
	s.Puts++
	s.Access[id] = m
	return nil
}
