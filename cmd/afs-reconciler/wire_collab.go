package main

import (
	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"context"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/wafv2"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// wireCollab registers the Paperclip collaboration jobs (RC-21..29) as leader-only periodic jobs: the
// EndpointReady controller, the link sync, the denied-spike detector, the board key bootstrap and the
// partition and session housekeeping.
//
// The managed-mode roster guard needs a Paperclip API client, which is an upstream dependency (RC-25 is
// parked), so it is not registered here; the board key finder is likewise absent, which keeps managed
// bootstrap pending. Both are listed under Parked in the spec.
func wireCollab(e jobs, runner *postgres.TxRunner, status engine.StatusWriter, outbox port.OutboxWriter, ids port.IDGen, clock port.Clock,
	awsCfg awssdk.Config, env func(string) string, now func() time.Time, log *slog.Logger) {
	db := postgres.NewCollabReconcile(runner, outbox, ids, clock, nil)
	ctl := &rcollab.Controller{Source: db, Status: status, Events: db, Now: now, Log: log}
	ls := &rcollab.LinkSync{Source: db, Events: db, Now: now, Log: log}
	sp := &rcollab.SpikeDetector{Source: db, Events: db, Now: now}
	bs := &rcollab.Bootstrap{Keys: db, Events: db, Log: log}
	e.AddJob(engine.Job{Name: "collab-endpoint", Every: 30 * time.Second, Immediate: true, Fn: ctl.Sync})
	e.AddJob(engine.Job{Name: "collab-links", Every: 30 * time.Second, Immediate: true, Fn: ls.Sync})
	e.AddJob(engine.Job{Name: "collab-denied-spike", Every: time.Minute, Immediate: false, Fn: sp.Run})
	e.AddJob(engine.Job{Name: "collab-bootstrap", Every: time.Minute, Immediate: true, Fn: bs.Run})
	wireCollabWAF(e, db, awsCfg, env, log)
	e.AddJob(engine.Job{Name: "collab-partitions", Every: 6 * time.Hour, Immediate: true,
		Fn: func(ctx context.Context) error { return rcollab.EnsurePartitions(ctx, db, now) }})
	e.AddJob(engine.Job{Name: "collab-sessions", Every: 10 * time.Minute, Immediate: false,
		Fn: func(ctx context.Context) error { return rcollab.PurgeSessions(ctx, db, log) }})
}

// Environment of the WAF allow-list sync (RC-28): the ARNs of the two IP sets Terraform creates. Without
// them the sync is off, with a warning, and the edge's own source check is the only filter.
const (
	envWAFIPSetV4 = "AFS_COLLAB_WAF_IPSET_V4_ARN"
	envWAFIPSetV6 = "AFS_COLLAB_WAF_IPSET_V6_ARN"
)

func wireCollabWAF(e jobs, db *postgres.CollabReconcile, awsCfg awssdk.Config, env func(string) string, log *slog.Logger) {
	v4, v6 := env(envWAFIPSetV4), env(envWAFIPSetV6)
	if v4 == "" && v6 == "" {
		log.Warn("the collaboration WAF allow-list sync is off: " + envWAFIPSetV4 + " and " + envWAFIPSetV6 + " are not set")
		return
	}
	w := &rcollab.WAFSync{CIDRs: db, Sets: wafv2.New(awsCfg), V4: rcollab.IPSetRef{ARN: v4}, V6: rcollab.IPSetRef{ARN: v6, IPv6: true}}
	e.AddJob(engine.Job{Name: "collab-waf", Every: 30 * time.Second, Immediate: true, Fn: w.Run})
}
