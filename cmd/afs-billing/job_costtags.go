package main

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/costexplorer"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
)

func init() { register("cost-tags", costTagCheck) }

// costTagCheck records, for each AFS tag key, whether the CUR has shown it and whether AWS Billing has it
// active, and raises cost_tags.not_seen for a key active for 3 days without the CUR showing it (M5-F1,
// BI-6). It reads the CUR rows cur-ingest stored, so it needs no Athena settings.
func costTagCheck(ctx context.Context, a *app, _ jobInput) error {
	c := &costtags.Checker{Store: postgres.NewCostTagsStore(), Tx: a.runner, Outbox: postgres.NewOutboxWriter(),
		Clock: realClock{}, IDs: ulidGen{}, Log: a.log, Session: postgres.BillingSession()}
	c.Billing = lazyBilling(func(ctx context.Context) (costtags.Billing, error) {
		cfg, err := a.aws(ctx)
		if err != nil {
			return nil, err
		}
		cfg.Region = "us-east-1" // Cost Explorer is a global service served from us-east-1
		return costexplorer.New(cfg), nil
	})
	_, err := c.Run(ctx)
	return err
}

// lazyBilling builds its costtags.Billing on first use, so an unreachable AWS configuration is a
// BillingErr (the CUR side is still recorded) and not a failure before the check starts.
type lazyBilling func(ctx context.Context) (costtags.Billing, error)

func (f lazyBilling) ListTags(ctx context.Context) (map[string]costtags.Activation, error) {
	b, err := f(ctx)
	if err != nil {
		return nil, err
	}
	return b.ListTags(ctx)
}
