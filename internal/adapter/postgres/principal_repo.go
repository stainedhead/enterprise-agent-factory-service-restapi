package postgres

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// lastLoginRefresh is how stale users.last_login_at may get before a request rewrites it, so a busy
// caller does not write its row on every request.
const lastLoginRefresh = 5 * time.Minute

// PrincipalRepo implements auth.ProviderSource and auth.PrincipalStore: the reads and writes that
// happen before a caller's team scope is known. users, role_bindings and identity_providers are
// readable to afs_app without a team scope (database contract §4); a person writes only their own users
// row (000013 policy app_write).
type PrincipalRepo struct {
	runner *TxRunner
	newID  func(now time.Time) (domain.ID, error)
}

// NewPrincipalRepo returns a PrincipalRepo. newID generates usr_ ids; nil uses crypto/rand.
func NewPrincipalRepo(runner *TxRunner, newID func(now time.Time) (domain.ID, error)) *PrincipalRepo {
	if newID == nil {
		newID = func(now time.Time) (domain.ID, error) { return domain.NewID("usr", now, nil) }
	}
	return &PrincipalRepo{runner: runner, newID: newID}
}

// EnabledProviders implements auth.ProviderSource.
func (r *PrincipalRepo) EnabledProviders(ctx context.Context) ([]auth.Provider, error) {
	var out []auth.Provider
	err := r.runner.ReadTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT id, kind, name, issuer_url, client_id, groups_claim, is_default
FROM afs.identity_providers WHERE enabled ORDER BY is_default DESC, created_at, id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (auth.Provider, error) {
			var p auth.Provider
			return p, r.Scan(&p.ID, &p.Kind, &p.Name, &p.IssuerURL, &p.ClientID, &p.GroupsClaim, &p.IsDefault)
		})
		return err
	})
	if err != nil {
		return nil, MapError(err, OpRead, "identity providers")
	}
	return out, nil
}

// SignIn implements auth.PrincipalStore: it finds the users row of (issuer, subject), creating it on
// first sign-in (just in time) and refreshing email, name and last_login_at when they changed. The
// transaction runs as the person (afs.actor = their id), which is what lets afs_app write the row.
func (r *PrincipalRepo) SignIn(ctx context.Context, u auth.UserClaims, now time.Time) (string, error) {
	var (
		id       string
		disabled bool
		email    string
		name     string
		lastSeen *time.Time
	)
	err := r.runner.ReadTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, `SELECT id, disabled_at IS NOT NULL, email, display_name, last_login_at
FROM afs.users WHERE oidc_issuer = $1 AND oidc_subject = $2`, u.Issuer, u.Subject).Scan(&id, &disabled, &email, &name, &lastSeen)
		if errors.Is(err, pgx.ErrNoRows) {
			id = ""
			return nil
		}
		return err
	})
	if err != nil {
		return "", MapError(err, OpRead, "user")
	}
	switch {
	case id == "":
		return r.create(ctx, u, now)
	case disabled:
		return "", domain.Unauthenticated()
	case email == u.Email && name == u.Name && lastSeen != nil && now.Sub(*lastSeen) < lastLoginRefresh:
		return id, nil
	}
	return id, r.refresh(ctx, id, u, now)
}

func (r *PrincipalRepo) refresh(ctx context.Context, id string, u auth.UserClaims, now time.Time) error {
	err := r.runner.InTx(ctx, port.Session{Actor: id, ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `UPDATE afs.users SET email = $2, display_name = $3, last_login_at = $4 WHERE id = $1`, id, u.Email, u.Name, now)
		return err
	})
	return r.signInError(err, u)
}

func (r *PrincipalRepo) create(ctx context.Context, u auth.UserClaims, now time.Time) (string, error) {
	newID, err := r.newID(now)
	if err != nil {
		return "", domain.Internal(err)
	}
	id := string(newID)
	err = r.runner.InTx(ctx, port.Session{Actor: id, ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `INSERT INTO afs.users (id, oidc_issuer, oidc_subject, email, display_name, last_login_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (oidc_issuer, oidc_subject) DO NOTHING`, id, u.Issuer, u.Subject, u.Email, u.Name, now)
		return err
	})
	if err != nil {
		return "", r.signInError(err, u)
	}
	// A concurrent first request may have won the insert: read back the winner's id.
	err = r.runner.ReadTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT id FROM afs.users WHERE oidc_issuer = $1 AND oidc_subject = $2`, u.Issuer, u.Subject).Scan(&id)
	})
	if err != nil {
		return "", MapError(err, OpRead, "user")
	}
	return id, nil
}

// signInError maps a write failure. A unique violation here is the email address already belonging to
// a person who signed in through another provider: the token is valid but cannot be matched to a
// person, so the caller gets 401 (the conflict is logged, never shown).
func (r *PrincipalRepo) signInError(err error, u auth.UserClaims) error {
	if err == nil {
		return nil
	}
	if domain.KindOf(err) == domain.KindConflict {
		slog.Warn("sign-in refused: email already belongs to another identity", "issuer", u.Issuer, "subject", u.Subject)
		return domain.Unauthenticated()
	}
	return MapError(err, OpWrite, "user")
}

// Bindings implements auth.PrincipalStore. The team's own provider and state need the team's rows,
// which row-level security shows only once the transaction names those teams, so they are read in a
// second step of the same read-only transaction.
func (r *PrincipalRepo) Bindings(ctx context.Context, userID string, groups []string) ([]auth.BindingRow, error) {
	if groups == nil {
		groups = []string{}
	}
	type row struct {
		role     string
		teamID   string
		group    string
		provider string
	}
	var rows []row
	teamInfo := map[string]struct{ provider, state string }{}
	s := port.Session{Actor: userID, ActorType: port.ActorUser}
	err := r.runner.ReadTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rs, err := t.Query(ctx, `SELECT role, coalesce(team_id, ''), coalesce(group_name, ''), coalesce(identity_provider_id, '')
FROM afs.role_bindings
WHERE (subject_type = 'user' AND user_id = $1) OR (subject_type = 'group' AND group_name = ANY($2))
ORDER BY created_at, id`, userID, groups)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(rs, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.role, &x.teamID, &x.group, &x.provider)
		})
		if err != nil {
			return err
		}
		var teamIDs []string
		seen := map[string]bool{}
		for _, x := range rows {
			if x.teamID != "" && !seen[x.teamID] {
				seen[x.teamID] = true
				teamIDs = append(teamIDs, x.teamID)
			}
		}
		if len(teamIDs) == 0 {
			return nil
		}
		if _, err := t.Exec(ctx, `SELECT set_config('afs.team_ids', $1, true)`, strings.Join(teamIDs, ",")); err != nil {
			return err
		}
		ts, err := t.Query(ctx, `SELECT id, coalesce(identity_provider_id, ''), state FROM afs.teams WHERE id = ANY($1)`, teamIDs)
		if err != nil {
			return err
		}
		defer ts.Close()
		for ts.Next() {
			var id, p, st string
			if err := ts.Scan(&id, &p, &st); err != nil {
				return err
			}
			teamInfo[id] = struct{ provider, state string }{p, st}
		}
		return ts.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "role bindings")
	}
	out := make([]auth.BindingRow, 0, len(rows))
	for _, x := range rows {
		b := auth.BindingRow{Role: authz.Role(x.role), TeamID: x.teamID, Group: x.group, BindingProviderID: x.provider}
		if x.teamID != "" {
			ti, ok := teamInfo[x.teamID]
			if !ok || ti.state == "deleted" {
				continue
			}
			b.TeamProviderID = ti.provider
		}
		out = append(out, b)
	}
	return out, nil
}

var (
	_ auth.ProviderSource = (*PrincipalRepo)(nil)
	_ auth.PrincipalStore = (*PrincipalRepo)(nil)
)
