package teardown_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/teardown"
)

type fakeStore struct {
	due        []teardown.Due
	finished   []teardown.Finished
	beginErr   map[string]error
	closeErr   map[string]error
	dueErr     error
	finErr     error
	notActive  map[string]bool
	began      []string
	beganAt    []time.Time
	closed     []string
	beginCalls int
}

func (f *fakeStore) Due(context.Context) ([]teardown.Due, error) { return f.due, f.dueErr }
func (f *fakeStore) Begin(_ context.Context, d teardown.Due, at time.Time) (bool, error) {
	f.beginCalls++
	if err := f.beginErr[d.TeamID]; err != nil {
		return false, err
	}
	if f.notActive[d.TeamID] {
		return false, nil
	}
	f.began, f.beganAt = append(f.began, d.TeamID), append(f.beganAt, at)
	return true, nil
}
func (f *fakeStore) Finished(context.Context) ([]teardown.Finished, error) {
	return f.finished, f.finErr
}
func (f *fakeStore) Close(_ context.Context, x teardown.Finished) error {
	if err := f.closeErr[x.TeamID]; err != nil {
		return err
	}
	f.closed = append(f.closed, x.TeamID)
	return nil
}

func TestJobRun(t *testing.T) {
	boom := errors.New("boom")
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		store      *fakeStore
		wantErr    bool
		wantBegan  []string
		wantClosed []string
		wantBegins int
	}{
		{"nothing due", &fakeStore{}, false, nil, nil, 0},
		{"due teams start, in order", &fakeStore{due: []teardown.Due{{TeamID: "team_a", Reason: "expired"}, {TeamID: "team_b", Reason: "cap_reached"}}}, false, []string{"team_a", "team_b"}, nil, 2},
		{"a team without a reason is not started", &fakeStore{due: []teardown.Due{{TeamID: "team_a"}}}, false, nil, nil, 0},
		{"a team no longer active is skipped quietly", &fakeStore{due: []teardown.Due{{TeamID: "team_a", Reason: "expired"}}, notActive: map[string]bool{"team_a": true}}, false, nil, nil, 1},
		{"one failure does not stop the others", &fakeStore{due: []teardown.Due{{TeamID: "team_a", Reason: "expired"}, {TeamID: "team_b", Reason: "expired"}}, beginErr: map[string]error{"team_a": boom}}, true, []string{"team_b"}, nil, 2},
		{"finished teams are closed", &fakeStore{finished: []teardown.Finished{{TeamID: "team_a", Reason: "expired"}, {TeamID: "team_b", Reason: "work_complete"}}}, false, nil, []string{"team_a", "team_b"}, 0},
		{"a close failure is reported and the rest close", &fakeStore{finished: []teardown.Finished{{TeamID: "team_a"}, {TeamID: "team_b"}}, closeErr: map[string]error{"team_a": boom}}, true, nil, []string{"team_b"}, 0},
		{"listing failure", &fakeStore{dueErr: boom}, true, nil, nil, 0},
		{"finished listing failure keeps the start errors", &fakeStore{due: []teardown.Due{{TeamID: "team_a", Reason: "expired"}}, finErr: boom}, true, []string{"team_a"}, nil, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := &teardown.Job{Store: tc.store, Now: func() time.Time { return now }}
			err := j.Run(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !equal(tc.store.began, tc.wantBegan) || !equal(tc.store.closed, tc.wantClosed) || tc.store.beginCalls != tc.wantBegins {
				t.Errorf("began %v closed %v calls %d", tc.store.began, tc.store.closed, tc.store.beginCalls)
			}
			for _, at := range tc.store.beganAt {
				if !at.Equal(now) {
					t.Errorf("began at %v", at)
				}
			}
		})
	}
}

type sched struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

func (s *sched) Every(name string, d time.Duration, fn func(context.Context) error) {
	s.name, s.interval, s.fn = name, d, fn
}

func TestRegisterRunsWithinFiveMinutes(t *testing.T) {
	s := &sched{}
	teardown.Register(s, &teardown.Job{Store: &fakeStore{}, Now: time.Now})
	if s.name == "" || s.fn == nil || s.interval <= 0 || s.interval > 5*time.Minute {
		t.Fatalf("registered %q every %v", s.name, s.interval)
	}
	if err := s.fn(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
