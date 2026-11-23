package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLister struct {
	models []CatalogModel
	err    error
}

func (f fakeLister) ListModels(context.Context) ([]CatalogModel, error) { return f.models, f.err }

type fakeModelStore struct {
	got []CatalogModel
	at  time.Time
	err error
}

func (f *fakeModelStore) Sync(_ context.Context, m []CatalogModel, at time.Time) (ModelSyncResult, error) {
	f.got, f.at = m, at
	return ModelSyncResult{Inserted: []string{m[0].ID}, Touched: len(m) - 1}, f.err
}

func TestModelSync(t *testing.T) {
	now := time.Date(2026, 11, 23, 3, 0, 0, 0, time.FixedZone("x", 3600))
	st := &fakeModelStore{}
	m := &ModelSync{Lister: fakeLister{models: []CatalogModel{{ID: "a"}, {ID: "b"}}}, Store: st, Clock: func() time.Time { return now }}
	if err := m.Run(t.Context()); err != nil || len(st.got) != 2 || !st.at.Equal(now) || st.at.Location() != time.UTC {
		t.Fatalf("run: %v %+v %v", err, st.got, st.at)
	}
	if err := (&ModelSync{Lister: fakeLister{}, Store: st}).Run(t.Context()); !errors.Is(err, ErrNoModels) {
		t.Errorf("empty list: %v", err)
	}
	if err := (&ModelSync{Lister: fakeLister{err: errors.New("denied")}, Store: st}).Run(t.Context()); err == nil {
		t.Error("list error must surface")
	}
	if err := (&ModelSync{Lister: fakeLister{models: []CatalogModel{{ID: "a"}}}, Store: &fakeModelStore{err: errors.New("db")}}).Run(t.Context()); err == nil {
		t.Error("store error must surface")
	}
	if j := ModelSyncJob(m); j.Interval != 24*time.Hour || j.Name != "bedrock-model-sync" {
		t.Errorf("job %+v", j)
	}
}
