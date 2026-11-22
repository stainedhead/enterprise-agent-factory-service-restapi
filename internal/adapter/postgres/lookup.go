package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// LookupActor is the afs.actor of the pre-scope lookups.
const LookupActor = "system:afs-api-lookup"

var lookupSession = port.Session{Actor: LookupActor, ActorType: port.ActorSystem}

// Credentials implements port.CredentialLookup and port.TeamDirectory with the SECURITY DEFINER
// functions of -db 000016 (X-DB-1..3). It replaces the interim lookup transaction of M0-F8.
type Credentials struct {
	runner *TxRunner
}

// NewCredentials returns Credentials on runner.
func NewCredentials(runner *TxRunner) *Credentials { return &Credentials{runner: runner} }

// AuthenticateAPIKey implements port.CredentialLookup. It runs read-write because the function
// records last_used_at.
func (c *Credentials) AuthenticateAPIKey(ctx context.Context, sum [32]byte) (port.APIKeyRecord, bool, error) {
	var (
		rec      port.APIKeyRecord
		found    bool
		teamID   *string
		teamSt   *string
		roleText *string
	)
	err := c.runner.InTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, `SELECT key_id, team_id, principal, role, roles, expires_at, revoked_at, state, team_state
FROM afs.authenticate_api_key($1)`, sum[:]).
			Scan(&rec.KeyID, &teamID, &rec.Principal, &roleText, &rec.Roles, &rec.ExpiresAt, &rec.RevokedAt, &rec.State, &teamSt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found {
		return port.APIKeyRecord{}, false, MapError(err, OpRead, "api key")
	}
	rec.TeamID, rec.TeamState, rec.Role = deref(teamID), deref(teamSt), deref(roleText)
	return rec, true, nil
}

// ResolveWorkload implements port.CredentialLookup.
func (c *Credentials) ResolveWorkload(ctx context.Context, namespace, serviceAccount string) (port.WorkloadRecord, bool, error) {
	var (
		rec          port.WorkloadRecord
		found        bool
		agent, poolI *string
	)
	err := c.runner.ReadTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, `SELECT team_id, agent_id, pool_id FROM afs.resolve_workload($1, $2)`, namespace, serviceAccount).
			Scan(&rec.TeamID, &agent, &poolI)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil || !found {
		return port.WorkloadRecord{}, false, MapError(err, OpRead, "workload")
	}
	rec.AgentID, rec.PoolID = deref(agent), deref(poolI)
	return rec, true, nil
}

// ListTeamDirectory implements port.TeamDirectory in the caller's own session; the function
// refuses (42501) unless the caller is a service admin or security operator.
func (c *Credentials) ListTeamDirectory(ctx context.Context, s port.Session) ([]port.TeamDirectoryEntry, error) {
	var out []port.TeamDirectoryEntry
	err := c.runner.ReadTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT id, slug, display_name FROM afs.team_directory()`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (port.TeamDirectoryEntry, error) {
			var e port.TeamDirectoryEntry
			return e, r.Scan(&e.ID, &e.Slug, &e.DisplayName)
		})
		return err
	})
	if err != nil {
		err = MapError(err, OpRead, "team directory")
		var de *domain.Error
		if errors.As(err, &de) && de.Kind == domain.KindAccessDenied {
			de.RequiredPermission = string(authz.TeamsReadDirectory)
		}
		return nil, err
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var (
	_ port.CredentialLookup = (*Credentials)(nil)
	_ port.TeamDirectory    = (*Credentials)(nil)
)
