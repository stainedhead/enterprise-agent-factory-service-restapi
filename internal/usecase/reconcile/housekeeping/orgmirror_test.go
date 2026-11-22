package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type fakeOrg struct {
	accounts []domain.OrgAccount
	err      error
}

func (f fakeOrg) ListAccounts(context.Context) ([]domain.OrgAccount, error) { return f.accounts, f.err }

// memStore is an in-memory mirror with the removal rule of the database store.
type memStore struct {
	rows       map[string]domain.OrgAccount
	referenced map[string]bool
	calls      int
}

func (s *memStore) Mirror(_ context.Context, accounts []domain.OrgAccount, at time.Time) (OrgResult, error) {
	s.calls++
	var res OrgResult
	seen := map[string]bool{}
	for _, a := range accounts {
		a.SyncedAt = at
		s.rows[a.ID] = a
		seen[a.ID] = true
		res.Upserted++
	}
	for id, a := range s.rows {
		if seen[id] {
			continue
		}
		if s.referenced[id] {
			a.Status = domain.OrgAccountSuspended
			s.rows[id] = a
			res.Retired = append(res.Retired, id)
		} else {
			delete(s.rows, id)
			res.Removed = append(res.Removed, id)
		}
	}
	return res, nil
}

func TestOrgMirrorRemovedAccountShowsInTheNextRun(t *testing.T) {
	store := &memStore{rows: map[string]domain.OrgAccount{}, referenced: map[string]bool{"222233334444": true}}
	a1 := domain.OrgAccount{ID: "111122223333", Name: "a", Status: domain.OrgAccountActive}
	a2 := domain.OrgAccount{ID: "222233334444", Name: "b", Status: domain.OrgAccountActive}
	a3 := domain.OrgAccount{ID: "333344445555", Name: "c", Status: domain.OrgAccountActive}
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	m := &OrgMirror{Store: store, Clock: func() time.Time { return now }}

	m.Lister = fakeOrg{accounts: []domain.OrgAccount{a1, a2, a3}}
	if err := m.Run(t.Context()); err != nil || len(store.rows) != 3 || !store.rows["111122223333"].SyncedAt.Equal(now) {
		t.Fatalf("first run: %v %+v", err, store.rows)
	}
	// a2 (still referenced by a link) and a3 leave the organization.
	m.Lister = fakeOrg{accounts: []domain.OrgAccount{a1}}
	if err := m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.rows["333344445555"]; ok {
		t.Error("removed account must be gone")
	}
	if store.rows["222233334444"].Status != domain.OrgAccountSuspended {
		t.Errorf("referenced account must be retired: %+v", store.rows["222233334444"])
	}
}

func TestOrgMirrorFailuresLeaveTheMirrorAlone(t *testing.T) {
	store := &memStore{rows: map[string]domain.OrgAccount{"111122223333": {ID: "111122223333"}}}
	m := &OrgMirror{Store: store, Lister: fakeOrg{err: errors.New("AccessDenied")}}
	if err := m.Run(t.Context()); err == nil || store.calls != 0 {
		t.Fatalf("list error: %v calls=%d", err, store.calls)
	}
	m.Lister = fakeOrg{}
	if err := m.Run(t.Context()); !errors.Is(err, ErrEmptyOrganization) || store.calls != 0 || len(store.rows) != 1 {
		t.Fatalf("empty: %v", err)
	}
	if j := OrgMirrorJob(m); j.Interval != time.Hour || j.Name == "" {
		t.Fatalf("job: %+v", j)
	}
}
