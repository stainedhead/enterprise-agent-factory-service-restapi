package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// VolumePollInterval is how soon an agent whose volume is not ready is looked at again.
const VolumePollInterval = 10 * time.Second

// Hooks are the integration points of the agent controller.
type Hooks struct {
	// Deleting handles an agent in state deleting (the ordered delete finalizer, lifecycle). Without it
	// the controller reports Deleting and touches nothing.
	Deleting func(ctx context.Context, d Desired) (engine.Outcome, error)
}

// Config configures the Controller. Source, Cluster, IAM and PodIdentity are required; Volumes and
// Snapshots enable expansion and the restore of a missing volume.
type Config struct {
	Source      Source
	Cluster     Cluster
	IAM         team.IAM
	PodIdentity PodIdentities
	Volumes     Volumes
	Snapshots   Snapshots
	AWS         team.AWSConfig
	Gateway     Gateway
	// Edge locates afs-edge (collaboration agent ingress).
	Edge  EdgeRef
	Hooks Hooks
	// LinkVerifierARN is the reconciler's role, which the dedicated agent roles trust so that role links
	// can be verified (M4-A4, links.TrustWithVerifier). Empty: links cannot be verified.
	LinkVerifierARN string
	// Events receives agent.model_unavailable; nil publishes nothing.
	Events Events
}

// Controller reconciles agents (engine.KindAgent).
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil || cfg.IAM == nil || cfg.PodIdentity == nil {
		return nil, errors.New("agent: Source, Cluster, IAM and PodIdentity are required")
	}
	if cfg.AWS.Partition == "" || cfg.AWS.AccountID == "" || cfg.AWS.Region == "" || cfg.AWS.SecretsPrefix == "" {
		return nil, errors.New("agent: AWS partition, account id, region and secrets prefix are required")
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
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (*Controller) Kind() engine.Kind { return engine.KindAgent }

// Reconcile implements engine.Controller. The IAM identity, the Kubernetes objects and the volume are
// independent: each is attempted, and a failure of one does not hide the others' conditions.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadAgent(ctx, ref.ID)
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
		// The team's delete finalizer owns the agent's teardown; applying now would re-create it.
		return engine.Outcome{Generation: d.Generation, Phase: domain.PhaseDeleting, NoStatus: true}, nil
	}

	var conds []domain.Condition
	var errs []error
	var changed []string

	// The tag set is checked before any AWS call. Above the cap nothing is applied, the agent keeps what
	// it has and shows TagsApplied false with the limit named (RC-9, TAG-4).
	want, tagErr := tags.Effective(tags.Subject{
		TeamID: d.Team.ID, TeamSlug: d.Team.Slug, AgentID: d.ID, AgentName: d.Name,
	}, d.Tags)
	if tagErr != nil {
		return engine.Outcome{Generation: d.Generation}, engine.WithCondition(tags.Condition, tagErr)
	}
	d.Tags = want

	roleARN, iamErr := c.reconcileIdentity(ctx, d)
	if iamErr != nil {
		errs = append(errs, engine.WithCondition(domain.CondIamReady, iamErr))
	} else {
		conds = append(conds, ok(domain.CondIamReady, "Reconciled"), ok(tags.Condition, "Applied"))
	}

	if d.Collab != nil {
		cc := *d.Collab
		cc.SecretsPrefix = c.cfg.AWS.SecretsPrefix + "/" + d.Team.Namespace + "/" + d.Name + "/paperclip/"
		d.Collab = &cc
	}
	env := Environment{
		Agent: d, RoleARN: roleARN, Region: c.cfg.AWS.Region, Gateway: c.cfg.Gateway.For(d.Team), Edge: c.cfg.Edge,
		Replicas: d.Replicas(), Hash: PodTemplateHash(d),
		NodePool: nodePool(d),
	}
	res, applyErr := c.cfg.Cluster.ApplyAgent(ctx, env)
	changed = append(changed, res.Changed...)
	if applyErr != nil {
		errs = append(errs, engine.WithCondition(domain.CondSecretsSynced, applyErr))
	} else {
		conds = append(conds, ok(domain.CondSecretsSynced, "Reconciled"))
	}

	requeue := time.Duration(0)
	phase := domain.PhaseReady
	if applyErr == nil {
		vc, volChanged, vErr := c.reconcileVolume(ctx, d)
		if volChanged {
			changed = append(changed, "PersistentVolumeClaim/"+d.Team.Namespace+"/"+PVCName(d.Name))
		}
		if vErr != nil {
			errs = append(errs, engine.WithCondition(domain.CondVolumeReady, vErr))
		} else {
			conds = append(conds, vc)
			if vc.Status != domain.ConditionTrue {
				phase, requeue = domain.PhaseReconciling, VolumePollInterval
			}
		}
	}
	conds = append(conds, templateCondition(d))
	conds = append(conds, ReportModel(ctx, c.cfg.Events, slog.Default(), ModelState{TeamID: d.Team.ID, Kind: "agents", ID: d.ID,
		Default: d.ModelID, Effective: d.Routes.Effective(d.Models), Reason: d.ModelReason, Prev: d.PrevModelUnavailable}))
	if d.Link != nil {
		conds = append(conds, keyCondition(d))
	}

	out := engine.Outcome{Generation: d.Generation, Conditions: conds, Changed: changed, Phase: phase, Requeue: requeue}
	if len(errs) > 0 {
		out.Phase = ""
		return out, errs[0]
	}
	out.Conditions = append(out.Conditions, ok("Ready", "Reconciled"))
	return out, nil
}

// reconcileIdentity ensures the agent's IAM identity and its Pod Identity association, and returns the role
// the pod assumes: the team role for team_shared, the agent's own role for dedicated (RC-2, AGT-6).
func (c *Controller) reconcileIdentity(ctx context.Context, d Desired) (string, error) {
	a := c.cfg.AWS
	path := team.RolePath(d.Team.Namespace)
	teamRole, err := team.TeamRoleName(d.Team.ID)
	if err != nil {
		return "", err
	}
	roleARN := team.RoleARN(a.Partition, a.AccountID, path, teamRole)
	podTags := tags.Set(d.Tags).WithApplied()
	podTags[team.TagManagedBy] = team.ManagedByValue
	if d.IAMIdentity == IdentityDedicated {
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
		// sts:AssumeRole on the exact ARN of every link that is not disabled (RC-3, RC-US-4.1).
		var targets []string
		for _, l := range d.RoleLinks {
			if l.Status != string(domain.RoleLinkStatusDisabled) {
				targets = append(targets, l.RoleARN)
			}
		}
		if policy, err = links.WithAssumeStatements(policy, targets); err != nil {
			return "", err
		}
		policies := map[string]string{"agent": policy}
		if d.IsCollaboration() {
			cp, err := team.CollaborationRolePolicy(a.SecretsPrefix, d.Team.Namespace, d.Name, d.Team.ID, a.KMSKeyARN)
			if err != nil {
				return "", err
			}
			policies["collaboration"] = cp
		}
		roleTags := cloneTags(podTags)
		if roleARN, err = c.cfg.IAM.EnsureRole(ctx, team.RoleSpec{
			Name: name, Path: path, Description: "AFS agent " + d.Name + " of team " + d.Team.Slug,
			TrustPolicy: trust, PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: roleTags,
			Policies: policies,
		}); err != nil {
			return "", fmt.Errorf("agent role: %w", err)
		}
	}
	if err := c.cfg.PodIdentity.Ensure(ctx, d.Team.Namespace, ServiceAccountName(d.Name), roleARN, podTags); err != nil {
		return roleARN, fmt.Errorf("pod identity association: %w", err)
	}
	return roleARN, nil
}

// reconcileVolume expands a grown volume and re-creates a missing one from the latest snapshot, then
// reports VolumeReady (RC-16).
func (c *Controller) reconcileVolume(ctx context.Context, d Desired) (domain.Condition, bool, error) {
	cond := domain.Condition{Type: domain.CondVolumeReady}
	if c.cfg.Volumes == nil {
		return ok(domain.CondVolumeReady, "NotManaged"), false, nil
	}
	st, err := c.cfg.Volumes.Volume(ctx, d)
	if err != nil {
		return cond, false, err
	}
	changed := false
	if !st.Exists {
		if c.cfg.Snapshots != nil {
			id, found, err := c.cfg.Snapshots.LatestReady(ctx, d.ID)
			if err != nil {
				return cond, false, fmt.Errorf("find latest snapshot: %w", err)
			}
			if found {
				if err := c.cfg.Volumes.Restore(ctx, d, id); err != nil {
					return cond, false, fmt.Errorf("restore volume from %s: %w", id, err)
				}
				return domain.Condition{Type: domain.CondVolumeReady, Status: domain.ConditionFalse, Reason: "Restoring",
					Message: "volume re-created from snapshot " + id}, true, nil
			}
		}
		return domain.Condition{Type: domain.CondVolumeReady, Status: domain.ConditionUnknown, Reason: "WaitingForPod",
			Message: "the volume is created when the pod first starts"}, false, nil
	}
	if st.RequestedGiB < d.Volume.SizeGiB {
		if changed, err = c.cfg.Volumes.Expand(ctx, d); err != nil {
			return cond, false, fmt.Errorf("expand volume to %d GiB: %w", d.Volume.SizeGiB, err)
		}
		return domain.Condition{Type: domain.CondVolumeReady, Status: domain.ConditionFalse, Reason: "Resizing",
			Message: fmt.Sprintf("expanding to %d GiB", d.Volume.SizeGiB)}, changed, nil
	}
	switch {
	case st.Resizing || (st.CapacityGiB > 0 && st.CapacityGiB < d.Volume.SizeGiB):
		return domain.Condition{Type: domain.CondVolumeReady, Status: domain.ConditionFalse, Reason: "Resizing",
			Message: fmt.Sprintf("expanding to %d GiB", d.Volume.SizeGiB)}, false, nil
	case !st.Bound:
		return domain.Condition{Type: domain.CondVolumeReady, Status: domain.ConditionFalse, Reason: "Binding", Message: "the claim is not bound yet"}, false, nil
	}
	return ok(domain.CondVolumeReady, "Bound"), false, nil
}

// templateCondition reports TemplateRetired: True while the agent's template is disabled or its digest is no
// longer allowed (the agent keeps running), False once it is allowed again (D54, RC-US-1.3).
func templateCondition(d Desired) domain.Condition {
	if d.TemplateRetired {
		return domain.Condition{Type: domain.CondTemplateRetired, Status: domain.ConditionTrue, Reason: "TemplateRetired", Message: d.TemplateReason}
	}
	return domain.Condition{Type: domain.CondTemplateRetired, Status: domain.ConditionFalse, Reason: "TemplateAllowed"}
}

// keyCondition reports CollaborationKeyMissing on a connected worker: True while no Paperclip agent key
// has a value (the pod still starts, without PAPERCLIP_AGENT_API_KEY), False once it does (RC-22).
func keyCondition(d Desired) domain.Condition {
	if d.KeyMissing() {
		return domain.Condition{Type: domain.CondCollaborationKeyMissing, Status: domain.ConditionTrue, Reason: "KeyMissing",
			Message: "the agent's Paperclip key has no value yet; the agent starts without " + CollabKeyTarget}
	}
	return domain.Condition{Type: domain.CondCollaborationKeyMissing, Status: domain.ConditionFalse, Reason: "KeyPresent"}
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

// nodePool describes the team's dedicated nodes: the team's tags, without the agent's id.
func nodePool(d Desired) NodePool {
	if d.Team.IsolationTier != TierDedicated {
		return NodePool{}
	}
	tags := cloneTags(d.Tags)
	delete(tags, TagAgentID)
	tags[team.TagTeamID] = d.Team.ID
	return NodePool{Dedicated: true, Tags: tags}
}
