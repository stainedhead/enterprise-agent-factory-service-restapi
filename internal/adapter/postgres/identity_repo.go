package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/accounts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// IdentityRepo implements identity.Repository on afs.agent_role_links and afs.agent_identities. The
// database guards a link: the agent must be dedicated and the account allowed (check_violation, 400);
// role, external id and agent cannot change.
type IdentityRepo struct{}

// NewIdentityRepo returns an IdentityRepo.
func NewIdentityRepo() *IdentityRepo { return &IdentityRepo{} }

var _ identity.Repository = (*IdentityRepo)(nil)

// Agent implements identity.Repository.
func (*IdentityRepo) Agent(ctx context.Context, tx port.Tx, teamID, agentID string) (identity.AgentInfo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return identity.AgentInfo{}, err
	}
	var a identity.AgentInfo
	var iam string
	err = pt.QueryRow(ctx, `SELECT a.iam_identity, t.namespace,
  coalesce((SELECT i.external_ref FROM afs.agent_identities i WHERE i.agent_id = a.id AND i.kind = 'aws_iam_role' ORDER BY i.created_at DESC LIMIT 1), '')
FROM afs.agents a JOIN afs.teams t ON t.id = a.team_id
WHERE a.id = $1 AND a.team_id = $2 AND a.state <> 'deleted' AND a.deleted_at IS NULL`, agentID, teamID).Scan(&iam, &a.Namespace, &a.RoleARN)
	a.IAMIdentity = domain.IAMIdentity(iam)
	return a, MapError(err, OpRead, "agent")
}

// AccountAllowed implements identity.Repository.
func (*IdentityRepo) AccountAllowed(ctx context.Context, tx port.Tx, teamID, accountID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT afs.account_allowed($1, $2)`, teamID, accountID).Scan(&ok)
	return ok, MapError(err, OpRead, "account")
}

// SecretInTeam implements identity.Repository.
func (*IdentityRepo) SecretInTeam(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.secrets WHERE id = $1 AND team_id = $2)`, secretID, teamID).Scan(&ok)
	return ok, MapError(err, OpRead, "secret")
}

const linkCols = `id, team_id, agent_id, account_id, role_arn, profile_name, external_id, max_session_seconds, status,
  last_verified_at, last_error, version, created_at, created_by, updated_at`

func scanLink(row pgx.Row) (domain.RoleLink, error) {
	var l domain.RoleLink
	var status string
	var verified *time.Time
	var lastErr *string
	var v int64
	err := row.Scan(&l.ID, &l.TeamID, &l.AgentID, &l.AccountID, &l.RoleARN, &l.ProfileName, &l.ExternalID, &l.MaxSessionSeconds,
		&status, &verified, &lastErr, &v, &l.CreatedAt, &l.CreatedBy, &l.UpdatedAt)
	l.Status, l.LastVerifiedAt, l.Version = domain.RoleLinkStatus(status), verified, domain.Version(v)
	if lastErr != nil {
		l.LastError = *lastErr
	}
	return l, err
}

// ListLinks implements identity.Repository.
func (*IdentityRepo) ListLinks(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.RoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+linkCols+` FROM afs.agent_role_links WHERE team_id = $1 AND agent_id = $2 AND id > $3 ORDER BY id LIMIT $4`,
		teamID, agentID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "role link")
	}
	defer rows.Close()
	out := []domain.RoleLink{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "role link")
		}
		out = append(out, l)
	}
	return out, MapError(rows.Err(), OpRead, "role link")
}

// GetLink implements identity.Repository.
func (*IdentityRepo) GetLink(ctx context.Context, tx port.Tx, teamID, agentID, linkID string, forUpdate bool) (domain.RoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.RoleLink{}, err
	}
	q := `SELECT ` + linkCols + ` FROM afs.agent_role_links WHERE id = $1 AND team_id = $2 AND agent_id = $3`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	l, err := scanLink(pt.QueryRow(ctx, q, linkID, teamID, agentID))
	return l, MapError(err, OpRead, "role link")
}

// InsertLink implements identity.Repository.
func (*IdentityRepo) InsertLink(ctx context.Context, tx port.Tx, l domain.RoleLink) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_role_links (id, team_id, agent_id, account_id, role_arn, profile_name, external_id, max_session_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, l.ID, l.TeamID, l.AgentID, l.AccountID, l.RoleARN, l.ProfileName, l.ExternalID, l.MaxSessionSeconds)
	return MapError(err, OpWrite, "role link")
}

// DeleteLink implements identity.Repository.
func (*IdentityRepo) DeleteLink(ctx context.Context, tx port.Tx, teamID, agentID, linkID string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.agent_role_links WHERE id = $1 AND team_id = $2 AND agent_id = $3 AND version = $4`, linkID, teamID, agentID, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "role link")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// RequestVerify implements identity.Repository: back to pending_trust, which notifies the reconciler.
func (*IdentityRepo) RequestVerify(ctx context.Context, tx port.Tx, teamID, agentID, linkID string) (domain.RoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.RoleLink{}, err
	}
	if _, err := pt.Exec(ctx, `UPDATE afs.agent_role_links SET status = 'pending_trust', last_error = NULL WHERE id = $1 AND team_id = $2 AND agent_id = $3`,
		linkID, teamID, agentID); err != nil {
		return domain.RoleLink{}, MapError(err, OpWrite, "role link")
	}
	l, err := scanLink(pt.QueryRow(ctx, `SELECT `+linkCols+` FROM afs.agent_role_links WHERE id = $1`, linkID))
	return l, MapError(err, OpRead, "role link")
}

const identityCols = `id, team_id, agent_id, kind, coalesce(external_ref, ''), coalesce(secret_id, ''), status, version, created_at, created_by, updated_at`

func scanIdentity(row pgx.Row) (domain.AgentIdentity, error) {
	var i domain.AgentIdentity
	var kind, status string
	var v int64
	err := row.Scan(&i.ID, &i.TeamID, &i.AgentID, &kind, &i.ExternalRef, &i.SecretID, &status, &v, &i.CreatedAt, &i.CreatedBy, &i.UpdatedAt)
	i.Kind, i.Status, i.Version = domain.AgentIdentityKind(kind), domain.AgentIdentityStatus(status), domain.Version(v)
	return i, err
}

// ListIdentities implements identity.Repository.
func (*IdentityRepo) ListIdentities(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.AgentIdentity, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+identityCols+` FROM afs.agent_identities WHERE team_id = $1 AND agent_id = $2 AND id > $3 ORDER BY id LIMIT $4`,
		teamID, agentID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "identity")
	}
	defer rows.Close()
	out := []domain.AgentIdentity{}
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "identity")
		}
		out = append(out, i)
	}
	return out, MapError(rows.Err(), OpRead, "identity")
}

// GetIdentity implements identity.Repository.
func (*IdentityRepo) GetIdentity(ctx context.Context, tx port.Tx, teamID, agentID, id string, forUpdate bool) (domain.AgentIdentity, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.AgentIdentity{}, err
	}
	q := `SELECT ` + identityCols + ` FROM afs.agent_identities WHERE id = $1 AND team_id = $2 AND agent_id = $3`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	i, err := scanIdentity(pt.QueryRow(ctx, q, id, teamID, agentID))
	return i, MapError(err, OpRead, "identity")
}

// InsertIdentity implements identity.Repository.
func (*IdentityRepo) InsertIdentity(ctx context.Context, tx port.Tx, i domain.AgentIdentity) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_identities (id, team_id, agent_id, kind, external_ref, secret_id)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`, i.ID, i.TeamID, i.AgentID, string(i.Kind), i.ExternalRef, i.SecretID)
	return MapError(err, OpWrite, "identity")
}

// DeleteIdentity implements identity.Repository.
func (*IdentityRepo) DeleteIdentity(ctx context.Context, tx port.Tx, teamID, agentID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.agent_identities WHERE id = $1 AND team_id = $2 AND agent_id = $3 AND version = $4`, id, teamID, agentID, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "identity")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// AccountsRepo implements accounts.Repository on afs.org_aws_accounts.
type AccountsRepo struct{}

// NewAccountsRepo returns an AccountsRepo.
func NewAccountsRepo() *AccountsRepo { return &AccountsRepo{} }

var _ accounts.Repository = (*AccountsRepo)(nil)

func collectAccounts(rows pgx.Rows) ([]domain.OrgAccount, error) {
	defer rows.Close()
	out := []domain.OrgAccount{}
	for rows.Next() {
		var a domain.OrgAccount
		var st string
		if err := rows.Scan(&a.ID, &a.Name, &st, &a.SyncedAt); err != nil {
			return nil, MapError(err, OpRead, "account")
		}
		a.Status = domain.OrgAccountStatus(st)
		out = append(out, a)
	}
	return out, MapError(rows.Err(), OpRead, "account")
}

// ListOrg implements accounts.Repository.
func (*AccountsRepo) ListOrg(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.OrgAccount, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, name, status, synced_at FROM afs.org_aws_accounts WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "account")
	}
	return collectAccounts(rows)
}

// ListAllowed implements accounts.Repository: active accounts that match the team's filter.
func (*AccountsRepo) ListAllowed(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.OrgAccount, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT a.id, a.name, a.status, a.synced_at FROM afs.org_aws_accounts a
WHERE a.id > $2 AND afs.account_allowed($1, a.id) ORDER BY a.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "account")
	}
	return collectAccounts(rows)
}

// TeamExists implements accounts.Repository.
func (*AccountsRepo) TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.teams WHERE id = $1 AND state <> 'deleted')`, teamID).Scan(&ok)
	return ok, MapError(err, OpRead, "team")
}
