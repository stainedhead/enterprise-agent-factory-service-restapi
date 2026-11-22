package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

// KubectlSource implements kubectl.Source for the reconciler login: a team's flag, state and namespace and
// the identity provider its people sign in through (identity providers are service-level, ADR-013).
type KubectlSource struct{ runner *TxRunner }

// NewKubectlSource returns a KubectlSource.
func NewKubectlSource(runner *TxRunner) *KubectlSource { return &KubectlSource{runner: runner} }

var _ kubectl.Source = (*KubectlSource)(nil)

// kubectlTeamSelect reads a team with its identity provider: the one it chose (teams.identity_provider_id)
// or, when it chose none, the service default. A disabled or absent provider reads as no provider.
const kubectlTeamSelect = `SELECT t.id, t.slug, t.namespace, t.state, t.viewer_kubectl, p.id, coalesce(p.is_default, false)
  FROM afs.teams t
  LEFT JOIN afs.identity_providers p ON p.enabled AND (p.id = t.identity_provider_id OR (t.identity_provider_id IS NULL AND p.is_default))`

func scanKubectlTeam(row pgx.Row) (kubectl.Team, error) {
	var t kubectl.Team
	var provider *string
	var isDefault bool
	if err := row.Scan(&t.ID, &t.Slug, &t.Namespace, &t.State, &t.ViewerKubectl, &provider, &isDefault); err != nil {
		return t, err
	}
	if provider != nil {
		t.Providers = []kubectl.Provider{{ID: *provider, IsDefault: isDefault}}
	}
	return t, nil
}

// LoadTeam implements kubectl.Source.
func (s *KubectlSource) LoadTeam(ctx context.Context, teamID string) (kubectl.Team, error) {
	var t kubectl.Team
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		pt, err := Unwrap(tx)
		if err != nil {
			return err
		}
		t, err = scanKubectlTeam(pt.QueryRow(ctx, kubectlTeamSelect+` WHERE t.id = $1`, teamID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("team")
		}
		return err
	})
	return t, MapError(err, OpRead, "team")
}

// ListViewerTeams implements kubectl.Source.
func (s *KubectlSource) ListViewerTeams(ctx context.Context) ([]kubectl.Team, error) {
	var out []kubectl.Team
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		pt, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := pt.Query(ctx, kubectlTeamSelect+` WHERE t.viewer_kubectl AND t.state IN ('active', 'suspended') ORDER BY t.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanKubectlTeam(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, MapError(err, OpRead, "teams")
}
