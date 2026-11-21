package postgres

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/idp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// IdPRepo implements idp.Repository on afs.identity_providers. CD-9: the console client id is the
// column client_id; the CLI client id is cli_client_id, which migration 000017 adds. Until the
// module in use has it, the CLI client id is not stored and reads back empty.
type IdPRepo struct{ hasCLI atomic.Bool }

// NewIdPRepo returns an IdPRepo.
func NewIdPRepo() *IdPRepo { return &IdPRepo{} }

var _ idp.Repository = (*IdPRepo)(nil)

func (r *IdPRepo) cliColumn(ctx context.Context, pt *Tx) (bool, error) {
	if r.hasCLI.Load() {
		return true, nil
	}
	var ok bool
	err := pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema='afs' AND table_name='identity_providers' AND column_name='cli_client_id')`).Scan(&ok)
	if err != nil {
		return false, MapError(err, OpRead, "identity provider")
	}
	if ok {
		r.hasCLI.Store(true)
	}
	return ok, nil
}

func (r *IdPRepo) selectSQL(ctx context.Context, pt *Tx) (string, error) {
	has, err := r.cliColumn(ctx, pt)
	if err != nil {
		return "", err
	}
	cli := `''`
	if has {
		cli = `coalesce(cli_client_id, '')`
	}
	return `SELECT id, kind, name, issuer_url, client_id, ` + cli + `, coalesce(client_secret_arn, ''), groups_claim,
  is_default, enabled, version, created_at, created_by, updated_at FROM afs.identity_providers`, nil
}

func scanIdP(row pgx.Row) (domain.IdentityProvider, error) {
	var p domain.IdentityProvider
	var kind string
	var v int64
	err := row.Scan(&p.ID, &kind, &p.Name, &p.IssuerURL, &p.ConsoleClientID, &p.CLIClientID, &p.ClientSecretARN,
		&p.GroupsClaim, &p.IsDefault, &p.Enabled, &v, &p.CreatedAt, &p.CreatedBy, &p.UpdatedAt)
	p.Kind, p.Version = domain.IdPKind(kind), domain.Version(v)
	return p, err
}

// List implements idp.Repository. Providers are service level.
func (r *IdPRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.IdentityProvider, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	sel, err := r.selectSQL(ctx, pt)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, sel+` WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "identity provider")
	}
	defer rows.Close()
	out := []domain.IdentityProvider{}
	for rows.Next() {
		p, err := scanIdP(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "identity provider")
		}
		out = append(out, p)
	}
	return out, MapError(rows.Err(), OpRead, "identity provider")
}

// Get implements idp.Repository.
func (r *IdPRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.IdentityProvider, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.IdentityProvider{}, err
	}
	sel, err := r.selectSQL(ctx, pt)
	if err != nil {
		return domain.IdentityProvider{}, err
	}
	q := sel + ` WHERE id=$1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanIdP(pt.QueryRow(ctx, q, id))
	return p, MapError(err, OpRead, "identity provider")
}

// TeamsUsing implements idp.Repository.
func (*IdPRepo) TeamsUsing(ctx context.Context, tx port.Tx, id string) ([]idp.TeamRef, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, slug, display_name FROM afs.teams WHERE identity_provider_id=$1 AND state <> 'deleted' ORDER BY id`, id)
	if err != nil {
		return nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	out := []idp.TeamRef{}
	for rows.Next() {
		var t idp.TeamRef
		if err := rows.Scan(&t.ID, &t.Slug, &t.DisplayName); err != nil {
			return nil, MapError(err, OpRead, "team")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "team")
}

// ClearDefault implements idp.Repository.
func (*IdPRepo) ClearDefault(ctx context.Context, tx port.Tx, exceptID string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.identity_providers SET is_default=false WHERE is_default AND id <> $1`, exceptID)
	return MapError(err, OpWrite, "identity provider")
}

// Insert implements idp.Repository.
func (r *IdPRepo) Insert(ctx context.Context, tx port.Tx, p domain.IdentityProvider) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	has, err := r.cliColumn(ctx, pt)
	if err != nil {
		return err
	}
	if has {
		_, err = pt.Exec(ctx, `INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, cli_client_id, client_secret_arn, groups_claim, is_default, enabled)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.ID, string(p.Kind), p.Name, p.IssuerURL, p.ConsoleClientID, nullable(p.CLIClientID),
			nullable(p.ClientSecretARN), p.GroupsClaim, p.IsDefault, p.Enabled)
	} else {
		_, err = pt.Exec(ctx, `INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, client_secret_arn, groups_claim, is_default, enabled)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, p.ID, string(p.Kind), p.Name, p.IssuerURL, p.ConsoleClientID,
			nullable(p.ClientSecretARN), p.GroupsClaim, p.IsDefault, p.Enabled)
	}
	return MapError(err, OpWrite, "identity provider")
}

// Update implements idp.Repository.
func (r *IdPRepo) Update(ctx context.Context, tx port.Tx, p domain.IdentityProvider, expect domain.Version) (domain.IdentityProvider, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.IdentityProvider{}, err
	}
	has, err := r.cliColumn(ctx, pt)
	if err != nil {
		return domain.IdentityProvider{}, err
	}
	cliSet := ``
	args := []any{p.ID, int64(expect), p.Name, p.ConsoleClientID, nullable(p.ClientSecretARN), p.GroupsClaim, p.IsDefault, p.Enabled}
	if has {
		cliSet = `, cli_client_id=$9`
		args = append(args, nullable(p.CLIClientID))
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.identity_providers SET name=$3, client_id=$4, client_secret_arn=$5, groups_claim=$6,
  is_default=$7, enabled=$8`+cliSet+` WHERE id=$1 AND version=$2`, args...)
	if err != nil {
		return domain.IdentityProvider{}, MapError(err, OpWrite, "identity provider")
	}
	if tag.RowsAffected() == 0 {
		return domain.IdentityProvider{}, r.missOrStale(ctx, pt, p.ID)
	}
	return r.Get(ctx, tx, p.ID, false)
}

// Delete implements idp.Repository.
func (r *IdPRepo) Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.identity_providers WHERE id=$1 AND version=$2`, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "identity provider")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, pt, id)
	}
	return nil
}

func (*IdPRepo) missOrStale(ctx context.Context, pt *Tx, id string) error {
	var one int
	err := pt.QueryRow(ctx, `SELECT 1 FROM afs.identity_providers WHERE id=$1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("identity provider")
	}
	if err != nil {
		return MapError(err, OpRead, "identity provider")
	}
	return domain.PreconditionFailed()
}
