package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/me"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MeRepo implements me.TeamSource and the team flag lookup of the route authorization step.
type MeRepo struct {
	runner *TxRunner
}

// NewMeRepo returns a MeRepo.
func NewMeRepo(runner *TxRunner) *MeRepo { return &MeRepo{runner: runner} }

// Teams implements me.TeamSource: the listed teams that exist and are not deleted. s must name them
// in TeamIDs, or row-level security hides them.
func (r *MeRepo) Teams(ctx context.Context, s port.Session, ids []string) ([]me.TeamInfo, error) {
	var out []me.TeamInfo
	err := r.runner.ReadTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT id, slug, may_manage_aws_accounts, viewer_kubectl, may_add_external_systems
FROM afs.teams WHERE id = ANY($1) AND state <> 'deleted'`, ids)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (me.TeamInfo, error) {
			var x me.TeamInfo
			return x, r.Scan(&x.ID, &x.Slug, &x.Flags.MayManageAwsAccounts, &x.Flags.ViewerKubectl, &x.Flags.MayAddExternalSystems)
		})
		return err
	})
	if err != nil {
		return nil, MapError(err, OpRead, "teams")
	}
	return out, nil
}

// TeamFlags returns the flags of a team the session names; ok is false when the team does not
// exist or is deleted. It feeds the conditional grants of the route authorization step.
func (r *MeRepo) TeamFlags(ctx context.Context, s port.Session, teamID string) (authz.TeamFlags, bool, error) {
	ts, err := r.Teams(ctx, s, []string{teamID})
	if err != nil || len(ts) == 0 {
		return authz.TeamFlags{}, false, err
	}
	return ts[0].Flags, true, nil
}

var _ me.TeamSource = (*MeRepo)(nil)
