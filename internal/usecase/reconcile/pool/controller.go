package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Hooks are the integration points of the pool controller.
type Hooks struct {
	// Deleting handles a pool in state deleting (the ordered delete finalizer, lifecycle). Without it the
	// controller reports Deleting and touches nothing.
	Deleting func(ctx context.Context, d Desired) (engine.Outcome, error)
}

// Config configures the Controller. Source, Cluster, IAM, PodIdentity and Queues are required; Probe and
// Recorder enable the verification of the pool's role links (without a Probe a due link is recorded
// failed with VerifierNotConfigured).
type Config struct {
	Source      Source
	Cluster     Cluster
	IAM         team.IAM
	PodIdentity agent.PodIdentities
	Queues      Queues
	Probe       links.Probe
	Recorder    LinkRecorder
	AWS         team.AWSConfig
	Gateway     agent.Gateway
	Hooks       Hooks
	// LinkVerifierARN is the reconciler's role, which the pool role trusts so that role links can be
	// verified (links.TrustWithVerifier). Empty: links cannot be verified.
	LinkVerifierARN string
	// EFSStorageClass is the storage class of the shared EFS volume.
	EFSStorageClass string
	Now             Clock
	// Events receives agent.model_unavailable for the pool; nil publishes nothing.
	Events agent.Events
}

// DefaultEFSStorageClass is the storage class of a shared volume when the platform names none.
const DefaultEFSStorageClass = "efs-sc"

// Controller reconciles pools (engine.KindPool).
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil || cfg.IAM == nil || cfg.PodIdentity == nil || cfg.Queues == nil {
		return nil, errors.New("pool: Source, Cluster, IAM, PodIdentity and Queues are required")
	}
	if cfg.AWS.Partition == "" || cfg.AWS.AccountID == "" || cfg.AWS.Region == "" || cfg.AWS.SecretsPrefix == "" {
		return nil, errors.New("pool: AWS partition, account id, region and secrets prefix are required")
	}
	if cfg.Gateway.Namespace == "" {
		cfg.Gateway.Namespace = team.DefaultGatewayNamespace
	}
	if cfg.Gateway.Name == "" {
		cfg.Gateway.Name = team.DefaultGatewayName
	}
	if cfg.Gateway.Port == 0 {
		cfg.Gateway.Port = team.DefaultGatewayPort
	}
	if cfg.EFSStorageClass == "" {
		cfg.EFSStorageClass = DefaultEFSStorageClass
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (*Controller) Kind() engine.Kind { return engine.KindPool }

// Routes are the change routes of the pool controller: the pool row is the resource, the rows below a pool
// reconcile it (a pool's id is in the payload of the tables that carry one), and a table with no pool id
// reconciles every pool of the team. Service-level catalogs fan out to every pool. The wiring merges them
// with the other controllers' routes (mergeRoutes).
func Routes() []engine.Route {
	children := []engine.Target{{Kind: engine.KindPool, Children: true}}
	routes := []engine.Route{{Table: "agent_pools", Targets: []engine.Target{{Kind: engine.KindPool, Self: true}}}}
	for _, t := range []string{
		"teams", "team_models", "pool_models", "team_inference_profiles", "pool_handoffs", "pool_secret_bindings", "pool_config_values", "pool_role_links",
		"external_system_optouts", "secrets", "namespace_config_values", "team_tags", "work_orders",
	} {
		routes = append(routes, engine.Route{Table: t, Targets: children})
	}
	for _, t := range []string{"agent_templates", "external_systems", "service_tags", "org_aws_accounts", "models", "model_prices", "service_settings"} {
		routes = append(routes, engine.Route{Table: t, FanOut: []engine.Kind{engine.KindPool}})
	}
	return append(routes, engine.GatewayRoutes(engine.KindPool, true, "pool_model_endpoints")...)
}

// Reconcile implements engine.Controller. The IAM identity, the queue, the Kubernetes objects and the
// links are independent: each is attempted, and a failure of one does not hide the others' conditions.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadPool(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	switch {
	case d.State == StateDeleted:
		return engine.Outcome{NoStatus: true}, nil
	case d.State == StateDeleting:
		if c.cfg.Hooks.Deleting == nil {
			return engine.Outcome{Generation: d.Generation, Phase: domain.PhaseDeleting, NoStatus: true}, nil
		}
		return c.cfg.Hooks.Deleting(ctx, d)
	case d.Team.State == "deleting" || d.Team.State == "deleted":
		// The team's delete finalizer owns the pool's teardown; applying now would re-create it.
		return engine.Outcome{Generation: d.Generation, Phase: domain.PhaseDeleting, NoStatus: true}, nil
	}

	var (
		conds []domain.Condition
		errs  []error
	)
	want, tagErr := tags.Effective(tags.Subject{TeamID: d.Team.ID, TeamSlug: d.Team.Slug, PoolID: d.ID, PoolName: d.Name}, d.Tags)
	if tagErr != nil {
		return engine.Outcome{Generation: d.Generation}, engine.WithCondition(tags.Condition, tagErr)
	}
	d.Tags = want

	if linkErr := c.syncLinkEligibility(ctx, &d); linkErr != nil {
		errs = append(errs, linkErr)
	}

	inbox, handoffs, qErr := c.queueNames(d)
	if qErr != nil {
		return engine.Outcome{Generation: d.Generation}, engine.WithCondition(domain.CondIamReady, qErr)
	}
	roleARN, iamErr := c.reconcileIdentity(ctx, d, inbox, handoffs)
	if iamErr != nil {
		errs = append(errs, engine.WithCondition(domain.CondIamReady, iamErr))
	} else {
		conds = append(conds, ok(domain.CondIamReady, "Reconciled"), ok(tags.Condition, "Applied"))
	}

	inboxURL, handoffURLs, inboxCond, inboxErr := c.reconcileQueues(ctx, d, roleARN, inbox, handoffs)
	if inboxCond != nil {
		conds = append(conds, *inboxCond)
	}
	if inboxErr != nil {
		errs = append(errs, engine.WithCondition(CondInboxReady, inboxErr))
	}

	pol, polErr := egress.PolicyFor(d.Team.ID, egress.Agent{ID: d.ID, State: d.State, Systems: d.Systems})
	env := Environment{
		Pool: d, RoleARN: roleARN, Region: c.cfg.AWS.Region, Gateway: c.cfg.Gateway, Replicas: d.Replicas(),
		Hash: PodTemplateHash(d, inboxURL, handoffURLs), InboxURL: inboxURL, HandoffURLs: handoffURLs,
		NodePool: nodePool(d), EFSStorageClass: c.cfg.EFSStorageClass, Egress: pol,
	}
	res, applyErr := c.cfg.Cluster.ApplyPool(ctx, env)
	changed := append([]string(nil), res.Changed...)
	if applyErr != nil {
		errs = append(errs, engine.WithCondition(domain.CondSecretsSynced, applyErr))
	} else {
		conds = append(conds, ok(domain.CondSecretsSynced, "Reconciled"))
		if polErr != nil {
			errs = append(errs, engine.WithCondition(domain.CondEgressPolicyReady, polErr))
			conds = append(conds, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionFalse, Reason: "InvalidEndpoint", Message: polErr.Error()})
		} else {
			conds = append(conds, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionTrue, Reason: "PolicyApplied", Message: allowedSummary(pol)})
		}
	}

	phase, requeue := domain.PhaseReady, time.Duration(0)
	if applyErr == nil {
		wc, wErr := c.workersCondition(ctx, d, env.Replicas)
		if wErr != nil {
			errs = append(errs, engine.WithCondition(CondWorkersReady, wErr))
		} else {
			conds = append(conds, wc)
			if wc.Status != domain.ConditionTrue {
				phase, requeue = domain.PhaseReconciling, WorkerPollInterval
			}
		}
	}

	// The links are verified last: the pool's role must carry the assume statements and trust the
	// reconciler before a verification can pass.
	if iamErr == nil {
		vWait, vConds, vErr := c.verifyLinks(ctx, d)
		conds = append(conds, vConds...)
		if vErr != nil {
			errs = append(errs, vErr)
		}
		requeue = soonest(requeue, vWait)
	}
	conds = append(conds, templateCondition(d))
	conds = append(conds, agent.ReportModel(ctx, c.cfg.Events, slog.Default(), agent.ModelState{TeamID: d.Team.ID, Kind: "pools", ID: d.ID,
		Default: d.ModelID, Effective: d.Models, Reason: d.ModelReason, Prev: d.PrevModelUnavailable}))

	out := engine.Outcome{Generation: d.Generation, Conditions: conds, Changed: changed, Phase: phase, Requeue: requeue}
	if len(errs) > 0 {
		out.Phase = ""
		return out, errs[0]
	}
	out.Conditions = append(out.Conditions, ok("Ready", "Reconciled"))
	return out, nil
}

// queueNames names the pool's inbox queue (empty when no pool hands work to it) and the inbox queues of the
// pools it hands work to.
func (c *Controller) queueNames(d Desired) (inbox string, handoffs map[string]string, err error) {
	handoffs = map[string]string{}
	if d.Receives() {
		if inbox, err = QueueName(d.Team.ID, d.ID); err != nil {
			return "", nil, err
		}
	}
	for _, p := range d.SendsTo {
		n, err := QueueName(d.Team.ID, p.ID)
		if err != nil {
			return "", nil, err
		}
		handoffs[p.Name] = n
	}
	return inbox, handoffs, nil
}

// roleARNOf is the ARN of a pool's role, from its id alone (the same formula the pool controller uses to
// create it), so a queue's policy can name the roles of pools that are not reconciled yet.
func (c *Controller) roleARNOf(namespace, poolID string) (string, error) {
	name, err := RoleName(poolID)
	if err != nil {
		return "", err
	}
	a := c.cfg.AWS
	return team.RoleARN(a.Partition, a.AccountID, team.RolePath(namespace), name), nil
}

// reconcileIdentity ensures the pool's own IAM role and its Pod Identity association, and returns the role
// the workers assume. The role is the team role's trust and secrets read, plus the exact queues it uses and
// the exact roles of its links (RC-11, RC-US-9.1, RC-US-9.3).
func (c *Controller) reconcileIdentity(ctx context.Context, d Desired, inbox string, handoffs map[string]string) (string, error) {
	a := c.cfg.AWS
	path := team.RolePath(d.Team.Namespace)
	name, err := RoleName(d.ID)
	if err != nil {
		return "", err
	}
	trust, err := team.TeamRoleTrust(a.AccountID, a.ClusterARN, "")
	if err != nil {
		return "", err
	}
	if trust, err = links.TrustWithVerifier(trust, c.cfg.LinkVerifierARN); err != nil {
		return "", err
	}
	policy, err := team.TeamRolePolicy(a.SecretsPrefix, d.Team.Namespace, d.Team.ID)
	if err != nil {
		return "", err
	}
	var targets []string
	for _, l := range d.RoleLinks {
		if l.Status != domain.RoleLinkStatusDisabled {
			targets = append(targets, l.RoleARN)
		}
	}
	if policy, err = links.WithAssumeStatements(policy, targets); err != nil {
		return "", err
	}
	var inboxARN string
	if inbox != "" {
		inboxARN = c.cfg.Queues.Locate(inbox).ARN
	}
	var sendARNs []string
	for _, n := range handoffs {
		sendARNs = append(sendARNs, c.cfg.Queues.Locate(n).ARN)
	}
	if policy, err = WithQueueStatements(policy, inboxARN, sendARNs); err != nil {
		return "", err
	}
	podTags := tags.Set(d.Tags).WithApplied()
	podTags[team.TagManagedBy] = team.ManagedByValue
	roleARN, err := c.cfg.IAM.EnsureRole(ctx, team.RoleSpec{
		Name: name, Path: path, Description: "AFS pool " + d.Name + " of team " + d.Team.Slug,
		TrustPolicy: trust, PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: cloneTags(podTags),
		Policies: map[string]string{"pool": policy},
	})
	if err != nil {
		return "", fmt.Errorf("pool role: %w", err)
	}
	if err := c.cfg.PodIdentity.Ensure(ctx, d.Team.Namespace, ServiceAccountName(d.Name), roleARN, podTags); err != nil {
		return roleARN, fmt.Errorf("pod identity association: %w", err)
	}
	return roleARN, nil
}

// reconcileQueues ensures the pool's inbox queue when it receives handoffs, with a policy that lets only the
// senders' roles send and only the pool's role receive (RC-US-9.2), and returns the URLs the pods get. A
// queue that cannot be ensured still gets its deterministic URL, so the pods start and send as soon as it
// exists.
func (c *Controller) reconcileQueues(ctx context.Context, d Desired, roleARN, inbox string, handoffs map[string]string) (string, map[string]string, *domain.Condition, error) {
	urls := make(map[string]string, len(handoffs))
	for pool, n := range handoffs {
		urls[pool] = c.cfg.Queues.Locate(n).URL
	}
	if inbox == "" {
		return "", urls, nil, nil
	}
	url := c.cfg.Queues.Locate(inbox).URL
	senders := make([]string, 0, len(d.ReceivesFrom))
	for _, p := range d.ReceivesFrom {
		arn, err := c.roleARNOf(d.Team.Namespace, p.ID)
		if err != nil {
			return url, urls, nil, err
		}
		senders = append(senders, arn)
	}
	if roleARN == "" {
		var err error
		if roleARN, err = c.roleARNOf(d.Team.Namespace, d.ID); err != nil {
			return url, urls, nil, err
		}
	}
	q, err := c.cfg.Queues.Ensure(ctx, QueueSpec{Name: inbox, Tags: cloneTags(d.Tags), ReceiverRoleARN: roleARN, SenderRoleARNs: senders})
	if err != nil {
		return url, urls, nil, fmt.Errorf("inbox queue %s: %w", inbox, err)
	}
	if q.URL != "" {
		url = q.URL
	}
	cond := ok(CondInboxReady, "Reconciled")
	cond.Message = "inbox " + q.Name
	return url, urls, &cond, nil
}

// workersCondition reports WorkersReady from the Deployment: True when every desired worker is ready (or
// none is desired), False with the count while workers start.
func (c *Controller) workersCondition(ctx context.Context, d Desired, replicas int32) (domain.Condition, error) {
	w, err := c.cfg.Cluster.Workers(ctx, d)
	if err != nil {
		return domain.Condition{}, err
	}
	switch {
	case replicas == 0:
		reason := "ScaledToZero"
		switch {
		case d.State == StatePaused:
			reason = "Paused"
		case d.Team.Suspended():
			reason = "TeamSuspended"
		}
		return ok(CondWorkersReady, reason), nil
	case !w.Exists:
		return domain.Condition{Type: CondWorkersReady, Status: domain.ConditionUnknown, Reason: "WaitingForDeployment"}, nil
	case w.Ready < replicas:
		return domain.Condition{Type: CondWorkersReady, Status: domain.ConditionFalse, Reason: "Scaling",
			Message: fmt.Sprintf("%d of %d workers ready", w.Ready, replicas)}, nil
	}
	cond := ok(CondWorkersReady, "AllReady")
	cond.Message = fmt.Sprintf("%d of %d workers ready", w.Ready, replicas)
	return cond, nil
}

// templateCondition reports TemplateRetired: True while the pool's template is disabled or its digest is no
// longer allowed (the pool keeps running), False once it is allowed again (D54).
func templateCondition(d Desired) domain.Condition {
	if d.TemplateRetired {
		return domain.Condition{Type: domain.CondTemplateRetired, Status: domain.ConditionTrue, Reason: "TemplateRetired", Message: d.TemplateReason}
	}
	return domain.Condition{Type: domain.CondTemplateRetired, Status: domain.ConditionFalse, Reason: "TemplateAllowed"}
}

func allowedSummary(p egress.AgentPolicy) string {
	if len(p.Rules) == 0 {
		return "no external system is on for this pool"
	}
	s := "allowed:"
	for i, r := range p.Rules {
		if i > 0 {
			s += ","
		}
		s += " " + r.Slug
	}
	return s
}

func ok(typ, reason string) domain.Condition {
	return domain.Condition{Type: typ, Status: domain.ConditionTrue, Reason: reason}
}

func cloneTags(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+3)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// nodePool describes the team's dedicated nodes: the team's tags, without the pool's id.
func nodePool(d Desired) agent.NodePool {
	if d.Team.IsolationTier != agent.TierDedicated {
		return agent.NodePool{}
	}
	tg := cloneTags(d.Tags)
	delete(tg, tags.KeyPoolID)
	delete(tg, tags.KeyPoolName)
	tg[team.TagTeamID] = d.Team.ID
	return agent.NodePool{Dedicated: true, Tags: tg}
}

func soonest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	}
	return b
}
