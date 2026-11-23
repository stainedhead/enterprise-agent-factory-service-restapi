package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// modelState reads, for an agent ("agent") or a pool ("pool"), why its default model is refused (empty when the
// default is among the effective models) and the ModelUnavailable status the last reconcile wrote (nil when
// there is none). The reason comes from afs.model_admission, the same first-failing-level the gateway reports.
func modelState(ctx context.Context, t *Tx, subjectKind, id, defaultModel string, effective []string) (reason string, prev *bool, err error) {
	if !slices.Contains(effective, defaultModel) {
		var r *string
		err := t.QueryRow(ctx, `SELECT reason FROM afs.model_admission($1, $2) WHERE model_id = $3`, subjectKind, id, defaultModel).Scan(&r)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = domain.ReasonModelNotEnabledForService // the model is not in the catalog
		case err != nil:
			return "", nil, MapError(err, OpRead, "model admission")
		case r != nil && *r != "":
			reason = *r
		default:
			reason = domain.ReasonModelNotEnabledForAgent // admitted but outside the view: a change racing the read
		}
	}
	kind := "agent"
	if subjectKind == "pool" {
		kind = "pool"
	}
	var conds []byte
	err = t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, kind, id).Scan(&conds)
	if errors.Is(err, pgx.ErrNoRows) {
		return reason, nil, nil
	}
	if err != nil {
		return "", nil, MapError(err, OpRead, "resource status")
	}
	var cs []domain.Condition
	if json.Unmarshal(conds, &cs) == nil {
		for _, c := range cs {
			if c.Type == "ModelUnavailable" {
				on := c.Status == domain.ConditionTrue
				prev = &on
			}
		}
	}
	return reason, prev, nil
}
