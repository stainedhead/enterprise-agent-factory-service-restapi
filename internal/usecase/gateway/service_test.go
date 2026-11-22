package gateway_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway/gatewaytest"
)

const (
	teamA   = "team_01JD0000000000000000000A00"
	teamB   = "team_01JD0000000000000000000B00"
	modelID = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	profA   = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/aaa"
	profB   = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/bbb"
	budget  = "bud_01JD0000000000000000000A00"
)

var now = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

type rig struct {
	dir     *gatewaytest.Directory
	tokens  *gatewaytest.Tokens
	store   *gatewaytest.Store
	spend   *gatewaytest.Spend
	thr     *gatewaytest.Thresholds
	usage   *gatewaytest.Usage
	svc     *gateway.Service
	invokes atomic.Int32
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{dir: gatewaytest.NewDirectory(), tokens: &gatewaytest.Tokens{IDs: map[string]gateway.TokenIdentity{}},
		store: gatewaytest.NewStore(), spend: gatewaytest.NewSpend(domain.MustMoney("10")), thr: gatewaytest.NewThresholds(), usage: &gatewaytest.Usage{}}
	r.dir.Put("10.0.0.1", gateway.PodInfo{Namespace: "afs-t-alpha", Name: "agent-bot-0", UID: "u1", ServiceAccount: "agent-bot", TeamID: teamA, AgentID: "agt_01"})
	r.dir.Put("10.0.0.2", gateway.PodInfo{Namespace: "afs-t-alpha", Name: "pool-w-7f9", UID: "u2", ServiceAccount: "pool-w", TeamID: teamA, PoolID: "pool_01"})
	r.dir.Put("10.0.1.1", gateway.PodInfo{Namespace: "afs-t-beta", Name: "agent-x-0", UID: "u3", ServiceAccount: "agent-x", TeamID: teamB, AgentID: "agt_02"})
	r.store.Teams[teamA] = gateway.TeamRecord{ID: teamA, Namespace: "afs-t-alpha", State: "active"}
	r.store.Teams[teamB] = gateway.TeamRecord{ID: teamB, Namespace: "afs-t-beta", State: "active"}
	r.store.Models[teamA+"\x00"+modelID] = gateway.ModelAccess{Enabled: true, ProfileARN: profA, MaxOutputTokens: 4096}
	r.store.Models[teamB+"\x00"+modelID] = gateway.ModelAccess{Enabled: true, ProfileARN: profB, MaxOutputTokens: 4096}
	r.store.PriceRows[modelID] = []gateway.Price{
		{EffectiveFrom: now.Add(-time.Hour), Input: domain.MustMoney("3"), Output: domain.MustMoney("15"), CacheRead: domain.MustMoney("0.3"), CacheWrite: domain.MustMoney("3.75")},
		{EffectiveFrom: now.Add(-48 * time.Hour), Input: domain.MustMoney("1"), Output: domain.MustMoney("5")},
	}
	r.store.Budgets[teamA] = gateway.BudgetRecord{ID: budget, Period: "monthly", StartsAt: now.Add(-90 * 24 * time.Hour), Thresholds: []int{50, 80, 100}, OnExhaust: "block", Amount: domain.MustMoney("10")}
	r.svc = gateway.NewService(gateway.Deps{Pods: r.dir, Tokens: r.tokens, Store: r.store, Spend: r.spend, Thresholds: r.thr, Usage: r.usage},
		gateway.Config{Partition: "aws", AccountID: "111122223333", FailClosedTimeout: 200 * time.Millisecond, Now: func() time.Time { return now }})
	return r
}

func req(ip string) gateway.Request {
	return gateway.Request{SourceIP: netip.MustParseAddr(ip), Op: gateway.OpConverse, ModelID: modelID, MaxTokens: 100, InputBytes: 300, RequestID: "req-1"}
}

func (r *rig) invoke(res gateway.Result, err error) gateway.InvokeFunc {
	return func(context.Context, gateway.Authorized) (gateway.Result, error) { r.invokes.Add(1); return res, err }
}

func ok(in, out int) gateway.Result {
	return gateway.Result{Usage: gateway.Usage{Input: in, Output: out}, UsageReported: true, Started: true}
}

func kind(err error) domain.ErrorKind { return domain.KindOf(err) }

func TestIdentity(t *testing.T) {
	cases := map[string]struct {
		mutate func(*rig, *gateway.Request)
		want   domain.ErrorKind
	}{
		"known pod":           {func(*rig, *gateway.Request) {}, ""},
		"unknown ip":          {func(_ *rig, q *gateway.Request) { q.SourceIP = netip.MustParseAddr("10.9.9.9") }, domain.KindUnauthenticated},
		"directory not ready": {func(r *rig, _ *gateway.Request) { r.dir.IsReady = false }, domain.KindUnavailable},
		"pod without team": {func(r *rig, _ *gateway.Request) {
			r.dir.Put("10.0.0.1", gateway.PodInfo{Namespace: "afs-t-alpha", Name: "p", AgentID: "agt_01"})
		}, domain.KindUnauthenticated},
		"pod without agent or pool": {func(r *rig, _ *gateway.Request) {
			r.dir.Put("10.0.0.1", gateway.PodInfo{Namespace: "afs-t-alpha", Name: "p", TeamID: teamA})
		}, domain.KindUnauthenticated},
		"token of the same pod": {func(r *rig, q *gateway.Request) {
			r.tokens.IDs["good"] = gateway.TokenIdentity{Namespace: "afs-t-alpha", PodName: "agent-bot-0", PodUID: "u1"}
			q.Token = "good"
		}, ""},
		"token of another pod": {func(r *rig, q *gateway.Request) {
			r.tokens.IDs["other"] = gateway.TokenIdentity{Namespace: "afs-t-beta", PodName: "agent-x-0", PodUID: "u3"}
			q.Token = "other"
		}, domain.KindUnauthenticated},
		"token with another uid": {func(r *rig, q *gateway.Request) {
			r.tokens.IDs["stale"] = gateway.TokenIdentity{Namespace: "afs-t-alpha", PodName: "agent-bot-0", PodUID: "old"}
			q.Token = "stale"
		}, domain.KindUnauthenticated},
		"invalid token":         {func(_ *rig, q *gateway.Request) { q.Token = "garbage" }, domain.KindUnauthenticated},
		"token review down":     {func(r *rig, q *gateway.Request) { r.tokens.Err = errors.New("apiserver down"); q.Token = "x" }, domain.KindUnavailable},
		"deleted team's old ip": {func(r *rig, _ *gateway.Request) { r.dir.Delete("10.0.0.1") }, domain.KindUnauthenticated},
		"team row missing":      {func(r *rig, _ *gateway.Request) { delete(r.store.Teams, teamA) }, domain.KindUnauthenticated},
		"team deleting": {func(r *rig, _ *gateway.Request) {
			r.store.Teams[teamA] = gateway.TeamRecord{ID: teamA, Namespace: "afs-t-alpha", State: "deleting"}
		}, domain.KindUnauthenticated},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			q := req("10.0.0.1")
			tc.mutate(r, &q)
			err := r.svc.Do(t.Context(), q, r.invoke(ok(10, 20), nil))
			if kind(err) != tc.want {
				t.Fatalf("err = %v, want kind %q", err, tc.want)
			}
			if tc.want != "" && r.invokes.Load() != 0 {
				t.Fatal("the model was invoked for a refused call")
			}
		})
	}
}

func TestIdentityWithoutReviewerRefusesAToken(t *testing.T) {
	r := newRig(t)
	svc := gateway.NewService(gateway.Deps{Pods: r.dir, Store: r.store, Spend: r.spend, Thresholds: r.thr, Usage: r.usage}, gateway.Config{Now: func() time.Time { return now }})
	q := req("10.0.0.1")
	q.Token = "t"
	if err := svc.Do(t.Context(), q, r.invoke(ok(1, 1), nil)); kind(err) != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestSuspendedTeam(t *testing.T) {
	r := newRig(t)
	r.store.Teams[teamA] = gateway.TeamRecord{ID: teamA, Namespace: "afs-t-alpha", State: "suspended"}
	err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindAccessDenied || de.Reason != domain.ReasonTeamSuspended || de.Details["reason"] != "team_suspended" {
		t.Fatalf("err = %#v", err)
	}
	if r.invokes.Load() != 0 || r.spend.Held() != 0 {
		t.Fatal("a suspended team's call reserved or invoked")
	}
	rows := r.usage.All()
	if len(rows) != 1 || rows[0].Outcome != gateway.OutcomeError {
		t.Fatalf("usage = %+v", rows)
	}
}

func TestModelChecks(t *testing.T) {
	cases := map[string]struct {
		mutate func(*rig)
		want   domain.ErrorKind
		usage  gateway.Outcome
	}{
		"enabled":     {func(*rig) {}, "", gateway.OutcomeOK},
		"not enabled": {func(r *rig) { delete(r.store.Models, teamA+"\x00"+modelID) }, domain.KindAccessDenied, gateway.OutcomeRejectedModel},
		"disabled": {func(r *rig) {
			r.store.Models[teamA+"\x00"+modelID] = gateway.ModelAccess{Enabled: false, ProfileARN: profA}
		}, domain.KindAccessDenied, gateway.OutcomeRejectedModel},
		"profile not made": {func(r *rig) { r.store.Models[teamA+"\x00"+modelID] = gateway.ModelAccess{Enabled: true} }, domain.KindUnavailable, ""},
		"no price":         {func(r *rig) { r.store.PriceRows[modelID] = nil }, domain.KindUnavailable, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			tc.mutate(r)
			err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(10, 20), nil))
			if kind(err) != tc.want {
				t.Fatalf("err = %v", err)
			}
			rows := r.usage.All()
			if tc.usage == "" {
				if len(rows) != 0 {
					t.Fatalf("usage = %+v", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != tc.usage {
				t.Fatalf("usage = %+v, want %s", rows, tc.usage)
			}
		})
	}
}

func TestATeamsPodNeverGetsAnotherTeamsProfile(t *testing.T) {
	r := newRig(t)
	r.store.Budgets[teamB] = gateway.BudgetRecord{ID: "bud_B", Period: "lifetime", StartsAt: now.Add(-time.Hour), Thresholds: []int{100}, Amount: domain.MustMoney("10")}
	var seen []string
	for _, ip := range []string{"10.0.0.1", "10.0.1.1"} {
		err := r.svc.Do(t.Context(), req(ip), func(_ context.Context, a gateway.Authorized) (gateway.Result, error) {
			seen = append(seen, a.ProfileARN+" "+a.RoleARN)
			return ok(1, 1), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{profA + " arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-01JD0000000000000000000A00-bedrock",
		profB + " arn:aws:iam::111122223333:role/afs/afs-t-beta/afs-01JD0000000000000000000B00-bedrock"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("seen %v, want %v", seen, want)
	}
	// A request naming a model only team B has is refused for team A.
	r.store.Models[teamB+"\x00other.model"] = gateway.ModelAccess{Enabled: true, ProfileARN: profB}
	q := req("10.0.0.1")
	q.ModelID = "other.model"
	if err := r.svc.Do(t.Context(), q, r.invoke(ok(1, 1), nil)); kind(err) != domain.KindAccessDenied {
		t.Fatalf("err = %v", err)
	}
}

func TestSuccessSettlesActualCostWithTheReservedBudgetAndPeriod(t *testing.T) {
	r := newRig(t)
	res := gateway.Result{Usage: gateway.Usage{Input: 1000, Output: 500, CacheRead: 2000, CacheWrite: 100}, UsageReported: true, Started: true}
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(res, nil)); err != nil {
		t.Fatal(err)
	}
	if len(r.spend.Settlements) != 1 {
		t.Fatalf("settlements = %+v", r.spend.Settlements)
	}
	s := r.spend.Settlements[0]
	// 1000x3 + 500x15 + 2000x0.3 + 100x3.75 = 3000+7500+600+375 per million = 0.011475 USD.
	if s.Actual.String() != "0.011475" || s.BudgetID != budget || !s.Period.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || s.HoldID == "" {
		t.Fatalf("settlement = %+v", s)
	}
	if r.spend.Held() != 0 || r.spend.Snapshot().Spent.String() != "0.011475" {
		t.Fatalf("counter = %+v held %d", r.spend.Snapshot(), r.spend.Held())
	}
	rows := r.usage.All()
	if len(rows) != 1 || rows[0].Outcome != gateway.OutcomeOK || rows[0].Cost.String() != "0.011475" || rows[0].AgentID != "agt_01" || rows[0].WorkerPod != "" ||
		rows[0].TagSetID != "ts_default" || rows[0].Usage.CacheRead != 2000 {
		t.Fatalf("usage = %+v", rows)
	}
	if r.spend.Holders[0] != "agent-bot-0" {
		t.Fatalf("holder = %v", r.spend.Holders)
	}
}

func TestLifetimeBudgetPeriodIsItsStart(t *testing.T) {
	r := newRig(t)
	start := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r.store.Budgets[teamA] = gateway.BudgetRecord{ID: budget, Period: "lifetime", StartsAt: start, Thresholds: []int{100}, Amount: domain.MustMoney("10")}
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil)); err != nil {
		t.Fatal(err)
	}
	if !r.spend.Settlements[0].Period.Equal(start) {
		t.Fatalf("period = %v", r.spend.Settlements[0].Period)
	}
}

func TestPoolWorkerCallRecordsPoolAndPod(t *testing.T) {
	r := newRig(t)
	if err := r.svc.Do(t.Context(), req("10.0.0.2"), r.invoke(ok(5, 5), nil)); err != nil {
		t.Fatal(err)
	}
	row := r.usage.All()[0]
	if row.PoolID != "pool_01" || row.WorkerPod != "pool-w-7f9" || row.AgentID != "" {
		t.Fatalf("row = %+v", row)
	}
}

func TestSettlementRules(t *testing.T) {
	cases := map[string]struct {
		res     gateway.Result
		err     error
		settled string
		outcome gateway.Outcome
	}{
		"success": {ok(1000, 1000), nil, "0.018000", gateway.OutcomeOK},
		"upstream error before the first event releases everything": {gateway.Result{}, &domain.Error{Kind: domain.KindUpstream}, "0.000000", gateway.OutcomeError},
		"error mid stream settles on the tokens seen": {gateway.Result{Usage: gateway.Usage{Input: 100, Output: 10}, UsageReported: true, Started: true},
			&domain.Error{Kind: domain.KindUpstream}, "0.000450", gateway.OutcomeError},
		"abort with no usage settles to the reservation":   {gateway.Result{Started: true}, context.Canceled, "RESERVED", gateway.OutcomeError},
		"success without usage settles to the reservation": {gateway.Result{Started: true}, nil, "RESERVED", gateway.OutcomeOK},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(tc.res, tc.err))
			if (err == nil) != (tc.err == nil) {
				t.Fatalf("err = %v", err)
			}
			if r.spend.Held() != 0 || len(r.spend.Settlements) != 1 {
				t.Fatalf("held %d settlements %d: every reservation is settled", r.spend.Held(), len(r.spend.Settlements))
			}
			want := tc.settled
			if want == "RESERVED" {
				want = r.spend.Reservs[0].String()
			}
			got := r.spend.Settlements[0].Actual
			if got.String() != trimZero(want) {
				t.Fatalf("settled %s, want %s", got, want)
			}
			if rows := r.usage.All(); len(rows) != 1 || rows[0].Outcome != tc.outcome {
				t.Fatalf("usage = %+v", rows)
			}
		})
	}
}

func trimZero(s string) string { return domain.MustMoney(s).String() }

func TestSettlementSurvivesAClientThatLeft(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	err := r.svc.Do(ctx, req("10.0.0.1"), func(context.Context, gateway.Authorized) (gateway.Result, error) {
		cancel()
		return gateway.Result{Usage: gateway.Usage{Input: 10, Output: 10}, UsageReported: true, Started: true}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(r.spend.Settlements) != 1 || r.spend.Held() != 0 {
		t.Fatalf("settlements %d held %d", len(r.spend.Settlements), r.spend.Held())
	}
}

func TestBudgetExceeded(t *testing.T) {
	r := newRig(t)
	r.spend.Spent = domain.MustMoney("9.999990")
	err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindBudgetExceeded {
		t.Fatalf("err = %v", err)
	}
	if de.Details["budgetId"] != budget || de.Details["period"] != "monthly" || de.Details["remainingUsd"] != "0.00001" && de.Details["remainingUsd"] != "0.00" {
		t.Fatalf("details = %v", de.Details)
	}
	if r.invokes.Load() != 0 {
		t.Fatal("the model was invoked")
	}
	rows := r.usage.All()
	if len(rows) != 1 || rows[0].Outcome != gateway.OutcomeRejectedBudget || !rows[0].Cost.IsNegative() && rows[0].Cost.Micros() != 0 {
		t.Fatalf("usage = %+v", rows)
	}
	r.svc.Wait()
	if r.thr.Count(domain.EventBudgetExhausted) != 1 {
		t.Fatalf("exhausted events = %d", r.thr.Count(domain.EventBudgetExhausted))
	}
}

func TestNoBudgetFailsClosed(t *testing.T) {
	r := newRig(t)
	delete(r.store.Budgets, teamA)
	err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
	if kind(err) != domain.KindBudgetExceeded || r.invokes.Load() != 0 {
		t.Fatalf("err = %v invokes %d", err, r.invokes.Load())
	}
}

func TestNotifyOnlyBudgetKeepsTheCallGoing(t *testing.T) {
	r := newRig(t)
	b := r.store.Budgets[teamA]
	b.OnExhaust = "notify"
	r.store.Budgets[teamA] = b
	r.spend.Spent = domain.MustMoney("10")
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1000, 1000), nil)); err != nil {
		t.Fatal(err)
	}
	r.svc.Wait()
	if len(r.spend.Settlements) != 1 || r.spend.Settlements[0].HoldID != "" || r.spend.Settlements[0].Actual.String() != "0.018" && r.spend.Settlements[0].Actual.String() != "0.01800" {
		t.Fatalf("settlements = %+v", r.spend.Settlements)
	}
	if r.thr.Count(domain.EventBudgetExhausted) != 1 {
		t.Fatal("exhausted not announced for a notify-only budget")
	}
}

func TestThresholdsOnceUnderConcurrency(t *testing.T) {
	r := newRig(t)
	// Each call spends 0.2 of 10 USD; 200 calls cross 50, 80 and 100 percent and exhaust the budget.
	r.spend.Limit = domain.MustMoney("10")
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			_ = r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(gateway.Result{Usage: gateway.Usage{Input: 0, Output: 13334}, UsageReported: true, Started: true}, nil))
		})
	}
	wg.Wait()
	r.svc.Wait()
	if n := r.thr.Count(domain.EventBudgetThresholdCrossed); n != 3 {
		t.Fatalf("threshold events = %d, want 3 (%+v)", n, r.thr.Events)
	}
	if n := r.thr.Count(domain.EventBudgetExhausted); n != 1 {
		t.Fatalf("exhausted events = %d, want 1", n)
	}
	seen := map[int]int{}
	for _, e := range r.thr.Events {
		if e.Event == domain.EventBudgetThresholdCrossed {
			seen[e.Threshold]++
		}
	}
	if seen[50] != 1 || seen[80] != 1 || seen[100] != 1 {
		t.Fatalf("per threshold = %v", seen)
	}
}

func TestFailClosed(t *testing.T) {
	cases := map[string]func(*rig){
		"store down":        func(r *rig) { r.store.Err = gatewaytest.ErrDB },
		"store hangs":       func(r *rig) { r.store.Block = true },
		"reservation down":  func(r *rig) { r.spend.Err = gatewaytest.ErrDB },
		"reservation hangs": func(r *rig) { r.spend.Block = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			mutate(r)
			start := time.Now()
			err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
			if kind(err) != domain.KindUnavailable {
				t.Fatalf("err = %v, want Unavailable", err)
			}
			if time.Since(start) > 2*time.Second || r.invokes.Load() != 0 {
				t.Fatalf("took %v, invokes %d: must reject quickly and send nothing", time.Since(start), r.invokes.Load())
			}
		})
	}
}

func TestSettleRetriesThenGivesUpWithoutFailingTheCall(t *testing.T) {
	r := newRig(t)
	r.spend.SettleErr = gatewaytest.ErrDB
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil)); err != nil {
		t.Fatalf("a settlement failure must not fail a finished call: %v", err)
	}
	if r.spend.Held() != 1 {
		t.Fatal("the hold should remain for the sweeper")
	}
}

func TestCounterIsEnsuredOncePerPeriod(t *testing.T) {
	r := newRig(t)
	for range 5 {
		if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if r.spend.Ensures != 1 {
		t.Fatalf("ensure_spend_counter called %d times", r.spend.Ensures)
	}
}

func TestReservationUsesTheRequestsOwnLimitCappedToTheModel(t *testing.T) {
	r := newRig(t)
	q := req("10.0.0.1")
	q.MaxTokens = 999999 // above the model's 4096
	q.InputBytes = 3000
	if err := r.svc.Do(t.Context(), q, func(_ context.Context, a gateway.Authorized) (gateway.Result, error) {
		if a.MaxOutputTokens != 4096 {
			t.Errorf("cap = %d", a.MaxOutputTokens)
		}
		return ok(1, 1), nil
	}); err != nil {
		t.Fatal(err)
	}
	// 4096 x 15/Mtok + 1000 x 3/Mtok = 0.06144 + 0.003 = 0.06444
	if got := r.spend.Reservs[0].String(); got != "0.06444" {
		t.Fatalf("reserved %s", got)
	}
}

func TestMetadataIsBuiltAndPassedOn(t *testing.T) {
	r := newRig(t)
	r.store.TagRows[teamA] = []gateway.Tag{{Key: "cost-center", Value: "CC1", Source: "service"}, {Key: "app", Value: "x", Source: "team"}, {Key: "afs-team-id", Value: teamA, Source: "system"}}
	var got map[string]string
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), func(_ context.Context, a gateway.Authorized) (gateway.Result, error) {
		got = a.Metadata
		return ok(1, 1), nil
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"afs-team-id": teamA, "afs-agent-id": "agt_01", "cost-center": "CC1", "app": "x"}
	if len(got) != len(want) {
		t.Fatalf("metadata = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("metadata[%s] = %q", k, got[k])
		}
	}
}

func TestAsyncSettleAnswersBeforeTheBooksCloseAndWaitDrains(t *testing.T) {
	r := newRig(t)
	r.svc = gateway.NewService(gateway.Deps{Pods: r.dir, Tokens: r.tokens, Store: r.store, Spend: r.spend, Thresholds: r.thr, Usage: r.usage},
		gateway.Config{Partition: "aws", AccountID: "111122223333", AsyncSettle: true, Now: func() time.Time { return now }})
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	r.svc.Wait()
	if len(r.spend.Settlements) != 1 || len(r.usage.All()) != 1 || r.spend.Held() != 0 {
		t.Errorf("settlements %d usage %d held %d", len(r.spend.Settlements), len(r.usage.All()), r.spend.Held())
	}
	err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(gateway.Result{}, &domain.Error{Kind: domain.KindUpstream, Message: "boom"}))
	r.svc.Wait()
	if kind(err) != domain.KindUpstream || len(r.spend.Settlements) != 2 {
		t.Errorf("err %v settlements %d", err, len(r.spend.Settlements))
	}
}
