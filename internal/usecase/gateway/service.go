package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Deps are the ports the Service uses.
type Deps struct {
	Pods       PodDirectory
	Tokens     TokenReviewer // nil when the optional token path is not configured
	Store      Store
	Spend      Spend
	Thresholds Thresholds
	Usage      UsageSink
	// SettleRecorder is optional: told of every settlement that falls back to the retry queue.
	SettleRecorder SettleRecorder
}

// Config configures the Service.
type Config struct {
	// Partition and AccountID are the gateway's own, to build the team's Bedrock role ARN.
	Partition, AccountID string
	// FailClosedTimeout bounds everything before the call reaches Bedrock; past it the call is 503 and
	// nothing was sent (GW-3). Default 2 s.
	FailClosedTimeout time.Duration
	// SettleTimeout bounds settlement after the call, which does not depend on the client staying
	// connected. Default 10 s.
	SettleTimeout time.Duration
	// AsyncSettle moves settlement, the usage row and the threshold check off the response path: the agent
	// gets its answer first and the books are closed right behind it (GW-8 added latency). The books are
	// the same either way; Wait drains them.
	AsyncSettle                                        bool
	SettleQueueSize                                    int
	SettleRetryWindow, SettleRetryBase, SettleRetryMax time.Duration
	Now                                                func() time.Time
	Log                                                *slog.Logger
}

// Service runs one model call from identification to settlement.
type Service struct {
	Deps
	cfg        Config
	pods       PodDirectory
	tokens     TokenReviewer
	store      Store
	spend      Spend
	thresholds Thresholds
	log        *slog.Logger
	crossed    crossedSet
	droppedLog droppedNotes
	ensured    sync.Map // budget|period -> time.Time
	wg         sync.WaitGroup
	queue      *settleQueue
}

// NewService returns a Service.
func NewService(d Deps, cfg Config) *Service {
	if cfg.FailClosedTimeout <= 0 {
		cfg.FailClosedTimeout = 2 * time.Second
	}
	if cfg.SettleTimeout <= 0 {
		cfg.SettleTimeout = 10 * time.Second
	}
	if cfg.SettleQueueSize <= 0 {
		cfg.SettleQueueSize = DefaultSettleQueueSize
	}
	if cfg.SettleRetryWindow <= 0 {
		cfg.SettleRetryWindow = DefaultSettleRetryWindow
	}
	if cfg.SettleRetryBase <= 0 {
		cfg.SettleRetryBase = DefaultSettleRetryBase
	}
	if cfg.SettleRetryMax <= 0 {
		cfg.SettleRetryMax = DefaultSettleRetryMax
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Service{Deps: d, cfg: cfg, pods: d.Pods, tokens: d.Tokens, store: d.Store, spend: d.Spend, thresholds: d.Thresholds, log: cfg.Log}
	s.queue = newSettleQueue(s)
	return s
}

// Wait blocks until the background work of finished calls (threshold events) is done; shutdown and
// tests call it.
func (s *Service) Wait() { s.wg.Wait() }

const ensureTTL = time.Minute

// Do identifies the caller, checks the team, the model and the budget, reserves the worst-case cost,
// runs invoke and settles. It returns nil when the call succeeded, and otherwise a *domain.Error the
// adapter maps to the gateway contract §7 response. Settlement always happens, also when the client
// is gone.
func (s *Service) Do(ctx context.Context, req Request, invoke InvokeFunc) error {
	started := s.cfg.Now()
	caller, err := s.identify(ctx, req)
	if err != nil {
		return err
	}
	pre, cancel := context.WithTimeout(ctx, s.cfg.FailClosedTimeout)
	defer cancel()

	rec := func(o Outcome, u Usage, cost domain.Money, tagSet string) {
		if s.Usage != nil {
			s.Usage.Record(record(caller, req, o, u, cost, started, s.cfg.Now(), tagSet))
		}
	}
	tagSet, _ := s.store.TagSetID(pre, caller.TeamID) // a failure is handled below, where the call needs it

	tm, err := s.checkTeam(pre, caller)
	if err != nil {
		if domain.KindOf(err) == domain.KindAccessDenied {
			rec(OutcomeError, Usage{}, domain.Money{}, tagSet)
		}
		return err
	}
	access, err := s.checkModel(pre, caller, req.ModelID)
	if err != nil {
		if domain.KindOf(err) == domain.KindAccessDenied {
			rec(OutcomeRejectedModel, Usage{}, domain.Money{}, tagSet)
		}
		return err
	}
	prices, err := s.store.Prices(pre, req.ModelID)
	if err != nil {
		return unavailable("database", err)
	}
	price, ok := EffectivePrice(prices, started)
	if !ok {
		return unavailable("the model has no effective price", nil)
	}
	tags, err := s.store.Tags(pre, caller.TeamID)
	if err != nil {
		return unavailable("database", err)
	}
	if tagSet == "" {
		if tagSet, err = s.store.TagSetID(pre, caller.TeamID); err != nil {
			return unavailable("database", err)
		}
	}

	cap := OutputCap(req.MaxTokens, access.MaxOutputTokens)
	amount := ReserveAmount(price, cap, req.InputBytes)
	budget, hold, period, err := s.reserve(pre, caller, req, amount, rec, tagSet)
	if err != nil {
		return err
	}

	roleARN, err := TeamRoleARN(s.cfg.Partition, s.cfg.AccountID, tm.Namespace, caller.TeamID)
	if err != nil {
		s.settle(ctx, caller, budget, period, hold, domain.MoneyFromMicros(0))
		return domain.Internal(err)
	}
	meta, dropped := BuildMetadata(caller, tags)
	s.droppedLog.note(s.log, caller.TeamID, dropped)
	res, callErr := invoke(ctx, Authorized{Caller: caller, ModelID: req.ModelID, ProfileARN: access.ProfileARN, RoleARN: roleARN,
		MaxOutputTokens: cap, Metadata: meta, DroppedTags: dropped, RequestID: req.RequestID})

	closeBooks := func() {
		sctx := ctx
		if s.cfg.AsyncSettle {
			sctx = context.WithoutCancel(ctx)
		}
		actual := SettleAmount(res, callErr, price, amount)
		ctr := s.settle(sctx, caller, budget, period, hold, actual)
		outcome := OutcomeOK
		if callErr != nil {
			outcome = OutcomeError
		}
		cost := actual
		if callErr != nil && !res.Started {
			cost = domain.MoneyFromMicros(0)
		}
		rec(outcome, res.Usage, cost, tagSet)
		if ctr != nil {
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SettleTimeout)
			defer cancel()
			s.recordThresholds(tctx, caller, budget, period, *ctr)
		}
	}
	if s.cfg.AsyncSettle {
		s.wg.Go(closeBooks)
		return asCallError(callErr)
	}
	closeBooks()
	return asCallError(callErr)
}

// asCallError passes a domain error through and wraps anything else as an upstream failure.
func asCallError(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &domain.Error{Kind: domain.KindUpstream, Message: "the model provider returned an error", Cause: err}
}

// reserve finds the budget and reserves amount. A refusal is 402 (or, for a notify-only budget, the
// call goes on without a hold); every failure to reach the database is 503 and nothing is sent.
func (s *Service) reserve(ctx context.Context, c Caller, req Request, amount domain.Money,
	rec func(Outcome, Usage, domain.Money, string), tagSet string) (BudgetRecord, Hold, time.Time, error) {
	b, err := s.store.Budget(ctx, c.TeamID)
	switch {
	case errors.Is(err, ErrNotFound):
		// A team without a budget has no cap to spend under: fail closed.
		rec(OutcomeRejectedBudget, Usage{}, domain.Money{}, tagSet)
		return BudgetRecord{}, Hold{}, time.Time{}, budgetExceeded(BudgetRecord{}, time.Time{}, Hold{})
	case err != nil:
		return BudgetRecord{}, Hold{}, time.Time{}, unavailable("database", err)
	}
	period := PeriodStart(b, s.cfg.Now())
	if err := s.ensure(ctx, b.ID, period); err != nil {
		return b, Hold{}, period, unavailable("database", err)
	}
	h, err := s.spend.Reserve(ctx, b.ID, period, amount, c.PodName)
	if err != nil {
		return b, Hold{}, period, unavailable("database", err)
	}
	if h.ID != "" {
		return b, h, period, nil
	}
	s.wg.Go(func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SettleTimeout)
		defer cancel()
		s.recordExhausted(tctx, c, b, period, h)
	})
	if b.OnExhaust == "notify" {
		return b, Hold{}, period, nil // notify-only: the call goes on and is counted when it settles
	}
	rec(OutcomeRejectedBudget, Usage{}, domain.Money{}, tagSet)
	return b, h, period, budgetExceeded(b, period, h)
}

func budgetExceeded(b BudgetRecord, period time.Time, h Hold) *domain.Error {
	remaining := h.Limit.Sub(h.Spent).Sub(h.Reserved)
	if remaining.IsNegative() {
		remaining = domain.MoneyFromMicros(0)
	}
	d := map[string]any{"budgetId": b.ID, "period": b.Period, "remainingUsd": remaining.String()}
	if !period.IsZero() {
		d["periodStart"] = period.UTC().Format(time.RFC3339)
	}
	return &domain.Error{Kind: domain.KindBudgetExceeded, Message: "the call does not fit under the team's budget", Details: d}
}

// ensure opens the period's counter once a minute per budget and period, not on every call.
func (s *Service) ensure(ctx context.Context, budgetID string, period time.Time) error {
	key := budgetID + "|" + period.UTC().Format(time.RFC3339)
	if v, ok := s.ensured.Load(key); ok && s.cfg.Now().Sub(v.(time.Time)) < ensureTTL {
		return nil
	}
	if err := s.spend.EnsureCounter(ctx, budgetID, period); err != nil {
		return err
	}
	s.ensured.Store(key, s.cfg.Now())
	return nil
}

// settle settles with the budget and period the call reserved against (GW-US-6.4), on a context the
// client cannot cancel, retrying briefly. It returns the counter after the settlement, nil on failure
// (the settlement then goes to the bounded retry queue; only when that gives up does the hold expire and
// the reconciler's sweeper release it without the cost).
func (s *Service) settle(ctx context.Context, c Caller, b BudgetRecord, period time.Time, h Hold, actual domain.Money) *Counter {
	if b.ID == "" {
		return nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SettleTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctr, err := s.spend.Settle(sctx, h.ID, b.ID, period, actual)
		if err == nil {
			return &ctr
		}
		lastErr = err
		select {
		case <-sctx.Done():
			attempt = 3
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	s.fallBack(&pendingSettle{ctx: context.WithoutCancel(sctx), teamID: c.TeamID, holdID: h.ID, budgetID: b.ID, period: period, actual: actual}, lastErr)
	return nil
}
