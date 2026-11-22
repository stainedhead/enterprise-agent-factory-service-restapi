package postgres

import (
	"context"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// OrgMirrorStore implements housekeeping.OrgStore on afs.org_aws_accounts for the afs_reconciler login
// (which may insert, update and delete there).
type OrgMirrorStore struct{ runner port.TxRunner }

// NewOrgMirrorStore returns an OrgMirrorStore on the reconciler's runner.
func NewOrgMirrorStore(runner port.TxRunner) *OrgMirrorStore { return &OrgMirrorStore{runner: runner} }

var _ housekeeping.OrgStore = (*OrgMirrorStore)(nil)

// Mirror implements housekeeping.OrgStore in one transaction. An account that left the organization is
// deleted; when a role link still references it (ON DELETE RESTRICT) it is marked suspended instead, so
// the allowed-accounts filter no longer matches it.
func (s *OrgMirrorStore) Mirror(ctx context.Context, accounts []domain.OrgAccount, syncedAt time.Time) (housekeeping.OrgResult, error) {
	var res housekeeping.OrgResult
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	err := s.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		res = housekeeping.OrgResult{}
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		for _, a := range accounts {
			if _, err := t.Exec(ctx, `INSERT INTO afs.org_aws_accounts (id, name, status, synced_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, status = EXCLUDED.status, synced_at = EXCLUDED.synced_at`,
				a.ID, a.Name, string(a.Status), syncedAt); err != nil {
				return err
			}
			res.Upserted++
		}
		rows, err := t.Query(ctx, `SELECT a.id, EXISTS (SELECT 1 FROM afs.agent_role_links l WHERE l.account_id = a.id)
FROM afs.org_aws_accounts a WHERE NOT (a.id = ANY ($1))`, ids)
		if err != nil {
			return err
		}
		type gone struct {
			id         string
			referenced bool
		}
		var all []gone
		for rows.Next() {
			var g gone
			if err := rows.Scan(&g.id, &g.referenced); err != nil {
				rows.Close()
				return err
			}
			all = append(all, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
		for _, g := range all {
			if g.referenced {
				if _, err := t.Exec(ctx, `UPDATE afs.org_aws_accounts SET status = 'suspended', synced_at = $2 WHERE id = $1`, g.id, syncedAt); err != nil {
					return err
				}
				res.Retired = append(res.Retired, g.id)
				continue
			}
			if _, err := t.Exec(ctx, `DELETE FROM afs.org_aws_accounts WHERE id = $1`, g.id); err != nil {
				return err
			}
			res.Removed = append(res.Removed, g.id)
		}
		return nil
	})
	return res, MapError(err, OpWrite, "org accounts")
}
