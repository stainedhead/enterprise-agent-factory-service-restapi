package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/kubeconfig"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// KubeconfigRepo implements kubeconfig.Repository for the afs_app login: row-level security shows a
// caller only the teams it is bound to (every team to a service admin), so a team the caller cannot see
// reads as not found.
type KubeconfigRepo struct{}

// NewKubeconfigRepo returns a KubeconfigRepo.
func NewKubeconfigRepo() *KubeconfigRepo { return &KubeconfigRepo{} }

var _ kubeconfig.Repository = (*KubeconfigRepo)(nil)

// Team implements kubeconfig.Repository. A team being deleted or deleted has no kubeconfig.
func (*KubeconfigRepo) Team(ctx context.Context, tx port.Tx, teamID string) (kubeconfig.Team, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return kubeconfig.Team{}, err
	}
	var t kubeconfig.Team
	err = pt.QueryRow(ctx, `SELECT slug, namespace, viewer_kubectl FROM afs.teams WHERE id = $1 AND state IN ('active', 'suspended')`, teamID).
		Scan(&t.Slug, &t.Namespace, &t.ViewerKubectl)
	if errors.Is(err, pgx.ErrNoRows) {
		return kubeconfig.Team{}, domain.NotFound("team")
	}
	return t, MapError(err, OpRead, "team")
}
