package main

import (
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/organizations"
	awssts "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// linksWiring is the wiring of the role link reconciler (M4-A4, M4-A5): the controller, its routes and a
// lister wrapper that adds one links resource per live team.
type linksWiring struct {
	Controller *links.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
}

// wireLinks builds the links controller. verifier is the reconciler's own role (AFS_RECONCILER_ROLE_ARN):
// without it the dedicated agent roles do not trust the reconciler and every link verification fails with
// VerifierNotConfigured, so the controller is built with no probe.
func wireLinks(cfg awssdk.Config, runner *postgres.TxRunner, pool postgres.Querier2, audit port.AuditWriter, outbox port.OutboxWriter,
	ids port.IDGen, aws team.AWSConfig, verifier string, now func() time.Time) (linksWiring, error) {
	store := postgres.NewLinksStore(runner, audit, outbox, ids)
	var probe links.Probe
	if verifier != "" {
		probe = awssts.NewProbe(cfg)
	}
	ctl, err := links.NewController(links.Config{
		Source: store, Recorder: store, Probe: probe, Now: now,
		AgentRoleARN: func(namespace, agentID string) string {
			name, err := agent.RoleName(agentID)
			if err != nil {
				return ""
			}
			return team.RoleARN(aws.Partition, aws.AccountID, team.RolePath(namespace), name)
		},
	})
	if err != nil {
		return linksWiring{}, err
	}
	return linksWiring{Controller: ctl, Routes: links.Routes(),
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewTeamLister(l, pool, links.Kind) }}, nil
}

// registerOrgMirror adds the hourly organization account mirror (M4-A1) to the engine's leader-only jobs.
// AFS_ORG_MIRROR=off disables it, for an account that is not the organization's management account or a
// delegated administrator.
func registerOrgMirror(s housekeeping.Scheduler, cfg awssdk.Config, runner port.TxRunner, env func(string) string, now func() time.Time, log *slog.Logger) {
	if env("AFS_ORG_MIRROR") == "off" {
		log.Info("organization account mirror disabled (AFS_ORG_MIRROR=off)")
		return
	}
	m := &housekeeping.OrgMirror{Lister: organizations.New(cfg), Store: postgres.NewOrgMirrorStore(runner), Clock: now, Log: log}
	housekeeping.Register(s, []housekeeping.Job{housekeeping.OrgMirrorJob(m)})
}
