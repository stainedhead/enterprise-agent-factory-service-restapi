package reconcile_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile/reconciletest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	day   = "2026-11-20"
)

var dayT = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func setup() (*reconcile.Reconciler, *reconciletest.Repo, *fakes.Outbox) {
	repo := reconciletest.New()
	ob := &fakes.Outbox{}
	r := &reconcile.Reconciler{
		Tx: &fakes.TxRunner{}, Outbox: ob, Clock: &fakes.Clock{T: time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)},
		IDs: &fakes.IDGen{}, Repo: repo, Session: port.SystemSession("afs-billing"),
	}
	return r, repo, ob
}

func TestVarianceOverThresholdPublishesOneEvent(t *testing.T) {
	r, repo, ob := setup()
	repo.Set(day, teamA, "100.00", "100.00") // within
	repo.Set(day, teamB, "100.00", "90.00")  // +11.1%

	for range 2 { // a rerun must not publish again
		res, err := r.RunDay(t.Context(), dayT)
		if err != nil {
			t.Fatal(err)
		}
		if res.Teams != 2 || res.OverThreshold != 1 {
			t.Fatalf("result = %+v", res)
		}
	}
	evs := ob.Events
	if len(evs) != 1 || evs[0].Type != domain.EventCostVarianceDetected || evs[0].Detail.TeamID != teamB {
		t.Fatalf("events = %+v", evs)
	}
	if d := evs[0].Detail.Data; d["day"] != day || d["gatewayUsd"] != "100.00" || d["billedUsd"] != "90.00" {
		t.Fatalf("data = %v", d)
	}
	if repo.Rollups[day] != 2 {
		t.Fatalf("rollups = %d", repo.Rollups[day])
	}
}

func TestVarianceThreshold(t *testing.T) {
	for _, tc := range []struct {
		name            string
		gateway, billed string
		status          string
		events          int
	}{
		{"exactly at threshold is ok", "105.00", "100.00", reconcile.StatusOK, 0},
		{"just over", "105.10", "100.00", reconcile.StatusOverThreshold, 1},
		{"gateway under billed counts both ways", "90.00", "100.00", reconcile.StatusOverThreshold, 1},
		{"metered but not billed", "10.00", "0", reconcile.StatusMissing, 0},
		{"nothing either side", "0", "0", reconcile.StatusOK, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, repo, ob := setup()
			repo.Set(day, teamA, tc.gateway, tc.billed)
			if _, err := r.RunDay(t.Context(), dayT); err != nil {
				t.Fatal(err)
			}
			if got := repo.Rows[day+"/"+teamA].Status; got != tc.status || len(ob.Events) != tc.events {
				t.Fatalf("status = %s, events = %d", got, len(ob.Events))
			}
		})
	}
}

func TestEventAgainAfterRecovery(t *testing.T) {
	r, repo, ob := setup()
	repo.Set(day, teamA, "100", "50")
	if _, err := r.RunDay(t.Context(), dayT); err != nil { // over
		t.Fatal(err)
	}
	repo.Set(day, teamA, "100", "100")
	if _, err := r.RunDay(t.Context(), dayT); err != nil { // back to ok
		t.Fatal(err)
	}
	repo.Set(day, teamA, "100", "50")
	if _, err := r.RunDay(t.Context(), dayT); err != nil { // over again: a new detection
		t.Fatal(err)
	}
	if n := len(ob.Events); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
}

func TestSkipsADayWithoutBilling(t *testing.T) {
	r, repo, ob := setup()
	repo.Gateway[day] = map[string]domain.Money{teamA: domain.MustMoney("10")} // metered, CUR not delivered
	res, err := r.RunDay(t.Context(), dayT)
	if err != nil || !res.Skipped || len(repo.Rows) != 0 || len(ob.Events) != 0 || repo.Rollups[day] != 0 {
		t.Fatalf("res=%+v err=%v rows=%v", res, err, repo.Rows)
	}
}

func TestFailureRollsBackTheDay(t *testing.T) {
	r, repo, ob := setup()
	repo.Set(day, teamA, "100", "50")
	repo.Set(day, teamB, "100", "50")
	repo.FailOn["Upsert"] = errors.New("db down")
	// The fake runner rolls back outbox writes when fn fails.
	if _, err := r.RunDay(t.Context(), dayT); err == nil {
		t.Fatal("want error")
	}
	if len(ob.Events) != 0 {
		t.Fatalf("events published for a failed day: %v", ob.Events)
	}
}

func TestRunContinuesAfterAFailedDay(t *testing.T) {
	r, repo, _ := setup()
	repo.Set("2026-11-19", teamA, "1", "1")
	repo.Set(day, teamA, "1", "1")
	repo.FailOn["Totals"] = errors.New("boom")
	got, err := r.Run(t.Context(), []time.Time{dayT.AddDate(0, 0, -1), dayT})
	if err == nil || len(got) != 2 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestVarianceMetric(t *testing.T) {
	r, repo, _ := setup()
	var seen []float64
	r.OnVariance = func(v float64) { seen = append(seen, v) }
	repo.Set(day, teamA, "110", "100")
	repo.Set(day, teamB, "100", "100")
	if _, err := r.RunDay(t.Context(), dayT); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] < 0.0999 || seen[0] > 0.1001 {
		t.Fatalf("variance = %v", seen)
	}
}

type failIDs struct{}

func (failIDs) NewID(domain.IDPrefix) (domain.ID, error) { return "", errors.New("no ids") }

type failOutbox struct{}

func (failOutbox) Publish(context.Context, port.Tx, domain.Event) error {
	return errors.New("outbox down")
}

// A failing event publish or id generator fails the day and writes nothing.
func TestPublishFailuresFailTheDay(t *testing.T) {
	for name, mut := range map[string]func(*reconcile.Reconciler){
		"ids":    func(r *reconcile.Reconciler) { r.IDs = failIDs{} },
		"outbox": func(r *reconcile.Reconciler) { r.Outbox = failOutbox{} },
	} {
		t.Run(name, func(t *testing.T) {
			r, repo, _ := setup()
			mut(r)
			repo.Set(day, teamA, "100", "50")
			if _, err := r.RunDay(t.Context(), dayT); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// Run logs a failed day and stops when the context is canceled.
func TestRunLogsAndStopsOnCancel(t *testing.T) {
	r, repo, _ := setup()
	var buf bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&buf, nil))
	repo.Set(day, teamA, "1", "1")
	repo.FailOn["Totals"] = errors.New("boom")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := r.Run(ctx, []time.Time{dayT, dayT.AddDate(0, 0, 1)})
	if err == nil || len(got) != 1 || !strings.Contains(buf.String(), "reconciliation failed") {
		t.Fatalf("got=%d err=%v log=%q", len(got), err, buf.String())
	}
}
