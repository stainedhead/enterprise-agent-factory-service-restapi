package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ModelSyncInterval is how often the Bedrock model catalog is synced (MOD-5): daily.
const ModelSyncInterval = 24 * time.Hour

// ErrNoModels is returned when Bedrock lists no usable model: the call is wrong (a missing grant answers with
// an error, not an empty list), so the catalog is left as it is.
var ErrNoModels = errors.New("housekeeping: Bedrock listed no models; the catalog is left unchanged")

// CatalogModel is a model Bedrock offers in the reconciler's region.
type CatalogModel struct {
	// ID is the model id the gateway and the catalog use (the Bedrock model id, with its version suffix).
	ID          string
	Provider    string
	DisplayName string
}

// ModelLister lists the text models Bedrock offers (adapter/aws/bedrock).
type ModelLister interface {
	ListModels(ctx context.Context) ([]CatalogModel, error)
}

// ModelSyncResult says what a sync changed.
type ModelSyncResult struct {
	// Inserted are the ids of models not in the catalog before: they are added disabled with origin
	// bedrock_sync, and a service admin decides whether to enable them.
	Inserted []string
	// Touched counts the models already in the catalog whose synced_at moved.
	Touched int
}

// ModelStore is the database side of the sync (afs.models).
type ModelStore interface {
	// Sync inserts the models the catalog lacks (enabled = false, origin = bedrock_sync) and sets synced_at on
	// the rest. It never changes enabled, prices or any column an admin set.
	Sync(ctx context.Context, models []CatalogModel, at time.Time) (ModelSyncResult, error)
}

// ModelSync is the daily job that discovers Bedrock models.
type ModelSync struct {
	Lister ModelLister
	Store  ModelStore
	Clock  func() time.Time
	Log    *slog.Logger
}

// Run syncs once.
func (m *ModelSync) Run(ctx context.Context) error {
	models, err := m.Lister.ListModels(ctx)
	if err != nil {
		return fmt.Errorf("housekeeping: list Bedrock models: %w", err)
	}
	if len(models) == 0 {
		return ErrNoModels
	}
	clock := m.Clock
	if clock == nil {
		clock = time.Now
	}
	res, err := m.Store.Sync(ctx, models, clock().UTC())
	if err != nil {
		return fmt.Errorf("housekeeping: sync the model catalog: %w", err)
	}
	if m.Log != nil {
		m.Log.InfoContext(ctx, "model catalog synced", "listed", len(models), "inserted", len(res.Inserted), "touched", res.Touched)
	}
	return nil
}

// ModelSyncJob returns the scheduled job.
func ModelSyncJob(m *ModelSync) Job {
	return Job{Name: "bedrock-model-sync", Interval: ModelSyncInterval, Run: m.Run}
}
