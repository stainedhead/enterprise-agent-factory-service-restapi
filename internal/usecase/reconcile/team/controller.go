package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

// Defaults of the gateway's location (gateway contract §2).
const (
	DefaultGatewayNamespace = "afs-system"
	DefaultGatewayName      = "afs-gateway"
	DefaultGatewayPort      = 8443
)

// AWSConfig holds the account-level inputs of the AWS side. None is hardcoded: Helm supplies them
// (architecture §7).
type AWSConfig struct {
	Partition string
	Region    string
	AccountID string
	// GatewayRoleARN is the only principal the team's Bedrock role trusts (AFS_GATEWAY_ROLE_ARN).
	GatewayRoleARN string
	// PermissionsBoundaryARN limits every role (AFS_PERMISSIONS_BOUNDARY_ARN).
	PermissionsBoundaryARN string
	// ClusterARN narrows the Pod Identity trust to this cluster; empty leaves it out.
	ClusterARN string
	// ESORoleARN is the External Secrets Operator's role, trusted by the team role; empty leaves it out.
	ESORoleARN string
	// SecretsPrefix is the Secrets Manager path prefix (service setting secrets.path_prefix).
	SecretsPrefix string
	// KMSKeyARN is the environment key that encrypts Secrets Manager secrets (AFS_KMS_KEY_ARN). Optional: when
	// empty the collaboration agent's role gets no kms statement and Paperclip's own secrets use the account's
	// Secrets Manager default key through its own grant.
	KMSKeyARN string
}

func (c AWSConfig) validate() error {
	var missing []string
	for name, v := range map[string]string{"Partition": c.Partition, "Region": c.Region, "AccountID": c.AccountID, "GatewayRoleARN": c.GatewayRoleARN, "SecretsPrefix": c.SecretsPrefix} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("team: AWS config is missing %v", missing)
	}
	return nil
}

// Hooks are the integration points of later work on the team lifecycle (M1-I) and workloads (M2, M5).
type Hooks struct {
	// Deleting handles a team in state deleting (ordered teardown). Without it the controller reports
	// Deleting and touches nothing.
	Deleting func(ctx context.Context, t Desired) (engine.Outcome, error)
	// Suspended runs after the environment was reconciled for a suspended team, to scale workloads
	// down.
	Suspended func(ctx context.Context, t Desired) error
}

// Config configures the Controller. Every port is required except Hooks fields.
type Config struct {
	Source   Source
	Cluster  Cluster
	IAM      IAM
	Profiles InferenceProfiles
	AWS      AWSConfig
	Gateway  PlatformSpec
	Hooks    Hooks
	Now      func() time.Time
}

// Controller reconciles teams (engine.KindTeam).
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil || cfg.IAM == nil || cfg.Profiles == nil {
		return nil, errors.New("team: Source, Cluster, IAM and Profiles are required")
	}
	if err := cfg.AWS.validate(); err != nil {
		return nil, err
	}
	if cfg.Gateway.GatewayNamespace == "" {
		cfg.Gateway.GatewayNamespace = DefaultGatewayNamespace
	}
	if cfg.Gateway.GatewayName == "" {
		cfg.Gateway.GatewayName = DefaultGatewayName
	}
	if cfg.Gateway.GatewayPort == 0 {
		cfg.Gateway.GatewayPort = DefaultGatewayPort
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (c *Controller) Kind() engine.Kind { return engine.KindTeam }

// Reconcile implements engine.Controller. The Kubernetes and the AWS side are independent: both are
// attempted, and a failure of one does not hide the other's result (the outcome carries the condition
// that succeeded together with the error).
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	t, err := c.cfg.Source.LoadTeam(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	switch t.State {
	case "deleted":
		return engine.Outcome{NoStatus: true}, nil
	case "deleting":
		if c.cfg.Hooks.Deleting == nil {
			return engine.Outcome{Generation: t.Generation, Phase: domain.PhaseDeleting, NoStatus: true}, nil
		}
		return c.cfg.Hooks.Deleting(ctx, t)
	}

	var conds []domain.Condition
	var errs []error
	var changed []string

	// The tag set is checked before any AWS call: above the cap nothing is applied and the team shows
	// TagsApplied false with the limit named (RC-9, TAG-4).
	teamTags, tagErr := tags.Effective(tags.Subject{TeamID: t.ID, TeamSlug: t.Slug}, t.Tags)
	var roleARN string
	if tagErr != nil {
		errs = append(errs, engine.WithCondition(tags.Condition, tagErr))
		roleARN = c.teamRoleARN(t)
	} else {
		var iamErr error
		roleARN, iamErr = c.reconcileAWS(ctx, t, teamTags)
		if iamErr != nil {
			errs = append(errs, engine.WithCondition(domain.CondIamReady, iamErr))
		} else {
			conds = append(conds, ok(domain.CondIamReady, "Reconciled"), ok(tags.Condition, "Applied"))
		}
	}

	res, k8sErr := c.cfg.Cluster.ApplyTeam(ctx, Environment{
		Team: t, TeamRoleARN: roleARN, Region: c.cfg.AWS.Region,
		GatewayNamespace: c.cfg.Gateway.GatewayNamespace, GatewayName: c.cfg.Gateway.GatewayName, GatewayPort: c.cfg.Gateway.GatewayPort,
	})
	changed = append(changed, res.Changed...)
	if k8sErr != nil {
		errs = append(errs, engine.WithCondition(domain.CondNamespaceReady, k8sErr))
	} else {
		conds = append(conds, ok(domain.CondNamespaceReady, "Reconciled"))
	}

	out := engine.Outcome{Generation: t.Generation, Conditions: conds, Changed: changed}
	if len(errs) > 0 {
		return out, errs[0] // the first failure names the condition; both are logged by the engine's retry
	}
	if t.State == "suspended" && c.cfg.Hooks.Suspended != nil {
		if err := c.cfg.Hooks.Suspended(ctx, t); err != nil {
			return out, err
		}
	}
	out.Conditions = append(out.Conditions, ok("Ready", "Reconciled"))
	return out, nil
}

func ok(typ, reason string) domain.Condition {
	return domain.Condition{Type: typ, Status: domain.ConditionTrue, Reason: reason}
}
