package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/billingpull"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Route billing jobs (v1.2.0, MEG-BL-1..3):
//
//	route-pull        daily: OpenAI, Azure and Google figures for the lookback days, OpenRouter for today
//	route-statements  monthly: Nous and OpenCode statements of the previous month (or --day's month)
//	route-capacity    daily: capacity routes' cost to agents and pools by token share
//	route-price-sync  daily: LiteLLM's price list into route price proposals
func init() {
	register("route-pull", routePull)
	register("route-statements", routeStatements)
	register("route-capacity", routeCapacity)
	register("route-price-sync", routePriceSync)
}

// providerTimeout bounds one provider request.
const providerTimeout = 60 * time.Second

func (a *app) routeJob(pullers ...routebill.Puller) *routebill.Job {
	return &routebill.Job{Tx: a.runner, Outbox: postgres.NewOutboxWriter(), Clock: realClock{}, IDs: ulidGen{},
		Store: postgres.NewRouteBillStore(), Pullers: pullers, Log: a.log, Session: postgres.BillingSession()}
}

// billingKeys reads a route's provider credential from <prefix>/_service/billing/<route>, building the
// Secrets Manager reader and reading the prefix once. The value lives only in the request it authorizes.
func (a *app) billingKeys() billingpull.Credential {
	var once sync.Once
	var reader *secretsmanager.Reader
	var prefix string
	var initErr error
	return func(ctx context.Context, ep routebill.Endpoint) (string, error) {
		once.Do(func() {
			cfg, err := a.aws(ctx)
			if err != nil {
				initErr = err
				return
			}
			reader = secretsmanager.NewReader(cfg)
			initErr = a.runner.ReadTx(ctx, postgres.BillingSession(), func(ctx context.Context, tx port.Tx) error {
				var err error
				prefix, err = postgres.NewRouteBillStore().SecretPrefix(ctx, tx)
				return err
			})
		})
		if initErr != nil {
			return "", initErr
		}
		c, err := reader.ReadBillingKey(ctx, prefix, ep.Slug)
		if err != nil {
			return "", err
		}
		return c.Reveal(), nil
	}
}

// dailyPullers are the providers pulled for past days; Azure and Google only when a route is configured.
func (a *app) dailyPullers(client *http.Client, key billingpull.Credential) []routebill.Puller {
	ps := []routebill.Puller{&billingpull.OpenAI{Client: client, Key: key}}
	if len(a.settings.AzureScopes) > 0 {
		ps = append(ps, &billingpull.Azure{Client: client, Token: key, Scopes: a.settings.AzureScopes})
	}
	if len(a.settings.GoogleExports) > 0 {
		ps = append(ps, &billingpull.Google{Client: client, Token: key, Exports: a.settings.GoogleExports})
	}
	return ps
}

func logRouteResult(a *app, job string, day string, res routebill.Result) {
	over := 0
	for _, v := range res.Variances {
		if v.Over {
			over++
		}
	}
	un := map[string]string{}
	for r, m := range res.Unattributed {
		un[r] = m.String()
	}
	a.log.Info("route billing", "job", job, "period", day, "compared", len(res.Variances), "overThreshold", over,
		"events", res.Events, "unattributedUsd", un, "notReconciled", res.NotReconciled)
}

// routePull runs the daily provider pulls. A failed day or provider is logged and fails the run after the
// others are done.
func routePull(ctx context.Context, a *app, in jobInput) error {
	client := &http.Client{Timeout: providerTimeout}
	key := a.billingKeys()
	var errs []error
	j := a.routeJob(a.dailyPullers(client, key)...)
	for _, d := range a.days(in) {
		res, err := j.PullDay(ctx, d)
		logRouteResult(a, "route-pull", d.Format(time.DateOnly), res)
		errs = append(errs, err)
	}
	if in.Day == nil { // OpenRouter reports today only
		today := a.now().UTC().Truncate(24 * time.Hour)
		res, err := a.routeJob(&billingpull.OpenRouter{Client: client, Key: key, Now: a.now}).PullDay(ctx, today)
		logRouteResult(a, "route-pull", today.Format(time.DateOnly)+" (openrouter)", res)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// statementMonth is the month route-statements reconciles: --day's month, else the previous month.
func statementMonth(now time.Time, in jobInput) time.Time {
	if in.Day != nil {
		return *in.Day
	}
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, -1, 0)
}

func routeStatements(ctx context.Context, a *app, in jobInput) error {
	month := statementMonth(a.now(), in)
	res, err := a.routeJob().ReconcileMonth(ctx, month)
	logRouteResult(a, "route-statements", month.Format("2006-01"), res)
	return err
}

func routeCapacity(ctx context.Context, a *app, in jobInput) error {
	j := a.routeJob()
	for _, d := range a.days(in) {
		rows, err := j.AllocateCapacity(ctx, d)
		if err != nil {
			return fmt.Errorf("%s: %w", d.Format(time.DateOnly), err)
		}
		a.log.Info("capacity allocated", "day", d.Format(time.DateOnly), "rows", len(rows))
	}
	return nil
}

// routePriceSync proposes route prices from LiteLLM's list. With pricing.price_list_sync off it ends before
// it fetches the list.
func routePriceSync(ctx context.Context, a *app, _ jobInput) error {
	on, err := postgres.NewPriceStore(a.runner).SyncEnabled(ctx)
	if err != nil || !on {
		return err
	}
	list, err := (&billingpull.LiteLLM{URL: a.settings.PriceListURL, Client: &http.Client{Timeout: providerTimeout}}).Fetch(ctx)
	if err != nil {
		return err
	}
	proposed, superseded, err := a.routeJob().ProposePrices(ctx, list)
	a.log.Info("route prices", "listed", len(list), "proposed", proposed, "superseded", superseded)
	return err
}
