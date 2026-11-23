package postgres

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// ModelSyncStore is housekeeping.ModelStore on the reconciler's runner.
type ModelSyncStore struct{ runner port.TxRunner }

// NewModelSyncStore returns a ModelSyncStore.
func NewModelSyncStore(runner port.TxRunner) *ModelSyncStore { return &ModelSyncStore{runner: runner} }

var _ housekeeping.ModelStore = (*ModelSyncStore)(nil)

// Sync implements housekeeping.ModelStore in one transaction. The reconciler may insert a model (disabled, origin
// bedrock_sync) and set synced_at; it may not change any other column (000022 column grants), so a model an admin
// enabled, priced or renamed is only touched.
func (s *ModelSyncStore) Sync(ctx context.Context, models []housekeeping.CatalogModel, at time.Time) (housekeeping.ModelSyncResult, error) {
	var res housekeeping.ModelSyncResult
	err := s.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		res = housekeeping.ModelSyncResult{}
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		for _, m := range models {
			ct, err := t.Exec(ctx, `INSERT INTO afs.models (id, provider, display_name, enabled, origin, synced_at)
VALUES ($1, $2, $3, false, 'bedrock_sync', $4) ON CONFLICT (id) DO NOTHING`, m.ID, m.Provider, m.DisplayName, at)
			if err != nil {
				return err
			}
			if ct.RowsAffected() == 1 {
				res.Inserted = append(res.Inserted, m.ID)
				continue
			}
			if _, err := t.Exec(ctx, `UPDATE afs.models SET synced_at = $2 WHERE id = $1`, m.ID, at); err != nil {
				return err
			}
			res.Touched++
		}
		return nil
	})
	return res, MapError(err, OpWrite, "model catalog")
}
