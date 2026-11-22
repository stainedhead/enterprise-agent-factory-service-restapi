package main

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/pricing"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
)

func init() { register("price-sync", priceSync) }

// priceSync reads the AWS Price List and proposes changed model prices (M4-D1, BI-3). With
// pricing.price_list_sync off it ends before it calls AWS.
func priceSync(ctx context.Context, a *app, _ jobInput) error {
	store := postgres.NewPriceStore(a.runner, a.settings.PriceListSync)
	s := &prices.Syncer{Store: store, Tx: a.runner, Outbox: postgres.NewOutboxWriter(), Clock: realClock{}, IDs: ulidGen{},
		Log: a.log, Session: postgres.BillingSession()}
	// The list is built only when the setting is on, so a switched-off sync needs no AWS credentials.
	s.List = lazyList(func(ctx context.Context) (prices.PriceList, error) {
		cfg, err := a.aws(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Region = a.settings.PricingRegion // the Price List API is served from a few regions only
		return pricing.New(cfg, pricing.Config{Region: a.settings.PriceRegion}, a.log), nil
	})
	_, err := s.Run(ctx)
	return err
}

// lazyList builds its PriceList on first Fetch.
type lazyList func(ctx context.Context) (prices.PriceList, error)

func (f lazyList) Fetch(ctx context.Context) ([]prices.ListPrice, error) {
	l, err := f(ctx)
	if err != nil {
		return nil, err
	}
	return l.Fetch(ctx)
}
