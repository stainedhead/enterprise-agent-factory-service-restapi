// Package logs reconciles a team's log delivery (RC-18): one Fluent Bit aggregator per team that the
// platform's node collector feeds by namespace, with one output per enabled log destination and a
// redaction filter in front of every output (RC-US-16.1 to RC-US-16.4).
//
// The reconciler renders references and never reads a credential: destination credentials and the
// team's bound secret values reach the aggregator through External Secrets into Secrets only the
// aggregator pod mounts. The only key material the reconciler holds is the team's redaction key,
// derived from a platform master key (redact.DeriveTeamKey); it is a platform secret, not a team one.
//
// The resource is the team: engine kind Kind, id = team id. A change of any of the team's
// log_destinations (and a full resync) reconciles it. Each destination also gets a resource_status row
// of kind StatusKindDestination, which is where the API reads the destination's status from.
package logs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/redact"
)

// Kind is the engine kind of a team's log delivery. Its id is the team id.
const Kind engine.Kind = "log_delivery"

// StatusKindDestination is the resource_status kind of one log destination.
const StatusKindDestination = "log_destination"

// Condition types.
const (
	// CondAggregatorReady is True once the aggregator's objects are applied.
	CondAggregatorReady = "AggregatorReady"
	// CondOutputsReady is True when every enabled destination has an output.
	CondOutputsReady = "OutputsReady"
)

// Destination is a log destination as the reconciler reads it. SecretARN is the Secrets Manager ARN of
// the referenced team secret ("" for none); the value is never read.
type Destination struct {
	ID        string
	Name      string
	Kind      domain.LogDestinationKind
	Config    map[string]string
	SecretARN string
	Enabled   bool
	Version   int64
}

// Desired is a team's log delivery state.
type Desired struct {
	TeamID    string
	Slug      string
	Namespace string
	// State is active, suspended, deleting or deleted.
	State        string
	Destinations []Destination
}

// Spec is what the cluster adapter renders: the enabled, valid destinations and the team's key.
type Spec struct {
	Team Desired
	// Outputs are the destinations that get an output, in id order.
	Outputs []Destination
	// RedactionKey is the team's redaction key (raw bytes), rendered into a Secret only the
	// aggregator mounts.
	RedactionKey []byte
	// Image is the Fluent Bit image.
	Image string
	// ReconcilerNamespace is the namespace of the reconciler, which may send sign-in records
	// (M2-H4) to the aggregator's forward port.
	ReconcilerNamespace string
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct {
	Changed []string
}

// Cluster applies and removes a team's aggregator. Implementations are idempotent.
type Cluster interface {
	// ApplyAggregator applies the aggregator for spec: its objects are replaced to match, so an output
	// of a deleted or disabled destination is gone from the rendered configuration.
	ApplyAggregator(ctx context.Context, spec Spec) (ApplyResult, error)
	// RemoveAggregator deletes the team's aggregator objects (the last destination was removed).
	RemoveAggregator(ctx context.Context, namespace, teamID string) (ApplyResult, error)
}

// Source reads a team's log delivery state.
type Source interface {
	// LoadLogs returns the team and all its destinations, or a domain NotFound error.
	LoadLogs(ctx context.Context, teamID string) (Desired, error)
}

// Config configures the Controller.
type Config struct {
	Source  Source
	Cluster Cluster
	// DestinationStatus writes each destination's resource_status row; optional.
	DestinationStatus engine.StatusWriter
	// MasterKey derives the teams' redaction keys (at least redact.MinKeyLen bytes).
	MasterKey []byte
	// Image is the Fluent Bit image (DefaultImage when empty).
	Image string
	// ReconcilerNamespace is where the reconciler runs (DefaultReconcilerNamespace when empty).
	ReconcilerNamespace string
	Now                 func() time.Time
}

// DefaultReconcilerNamespace is the reconciler's namespace when none is configured.
const DefaultReconcilerNamespace = "afs-system"

// DefaultImage is the Fluent Bit image used when none is configured (pinned; Helm overrides it).
const DefaultImage = "cr.fluentbit.io/fluent/fluent-bit:3.2.2"

// Controller reconciles Kind.
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil {
		return nil, errors.New("logs: Source and Cluster are required")
	}
	if len(cfg.MasterKey) < redact.MinKeyLen {
		return nil, errors.New("logs: MasterKey must be at least 16 bytes")
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.ReconcilerNamespace == "" {
		cfg.ReconcilerNamespace = DefaultReconcilerNamespace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (c *Controller) Kind() engine.Kind { return Kind }

// Route routes changes of log_destinations to the team's log delivery.
func Route() engine.Route {
	return engine.Route{Table: "log_destinations", Targets: []engine.Target{{Kind: Kind}}}
}

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadLogs(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	switch d.State {
	case "deleted", "deleting":
		// The namespace's teardown removes the aggregator with everything else.
		return engine.Outcome{NoStatus: true}, nil
	}

	var outputs []Destination
	var refused []error
	invalid := map[string]error{}
	for _, dest := range d.Destinations {
		if !dest.Enabled {
			continue
		}
		if err := Validate(dest); err != nil {
			invalid[dest.ID] = err
			refused = append(refused, fmt.Errorf("destination %s (%s): %w", dest.Name, dest.ID, err))
			continue
		}
		outputs = append(outputs, dest)
	}

	var res ApplyResult
	conds := []domain.Condition{}
	if len(outputs) == 0 {
		res, err = c.cfg.Cluster.RemoveAggregator(ctx, d.Namespace, d.TeamID)
		if err != nil {
			return engine.Outcome{}, engine.WithCondition(CondAggregatorReady, err)
		}
		conds = append(conds, ok(CondAggregatorReady, "NoDestinations"))
	} else {
		key, err := redact.DeriveTeamKey(c.cfg.MasterKey, d.TeamID)
		if err != nil {
			return engine.Outcome{}, engine.Terminal(err)
		}
		res, err = c.cfg.Cluster.ApplyAggregator(ctx, Spec{Team: d, Outputs: outputs, RedactionKey: key, Image: c.cfg.Image, ReconcilerNamespace: c.cfg.ReconcilerNamespace})
		if err != nil {
			return engine.Outcome{Changed: res.Changed}, engine.WithCondition(CondAggregatorReady, err)
		}
		conds = append(conds, ok(CondAggregatorReady, "Applied"))
	}
	c.reportDestinations(ctx, d, invalid, len(outputs) > 0)
	out := engine.Outcome{Conditions: conds, Changed: res.Changed}
	if len(refused) > 0 {
		out.Conditions = append(out.Conditions, domain.Condition{Type: CondOutputsReady, Status: domain.ConditionFalse, Reason: "InvalidDestination", Message: errors.Join(refused...).Error()})
		return out, engine.Terminal(engine.WithCondition(CondOutputsReady, errors.Join(refused...)))
	}
	out.Conditions = append(out.Conditions, ok(CondOutputsReady, "Applied"))
	return out, nil
}

func ok(typ, reason string) domain.Condition {
	return domain.Condition{Type: typ, Status: domain.ConditionTrue, Reason: reason}
}

// reportDestinations writes each destination's status: Ready with DeliveryHealthy True once its output
// is in the applied configuration, Unknown when it is disabled, False when its settings were refused.
func (c *Controller) reportDestinations(ctx context.Context, d Desired, invalid map[string]error, applied bool) {
	if c.cfg.DestinationStatus == nil {
		return
	}
	now := c.cfg.Now()
	for _, dest := range d.Destinations {
		gen := dest.Version
		cond := domain.Condition{Type: domain.CondDeliveryHealthy, Status: domain.ConditionTrue, Reason: "OutputApplied"}
		phase := domain.PhaseReady
		switch {
		case invalid[dest.ID] != nil:
			cond = domain.Condition{Type: domain.CondDeliveryHealthy, Status: domain.ConditionFalse, Reason: "InvalidDestination", Message: invalid[dest.ID].Error()}
			phase = domain.PhaseFailed
		case !dest.Enabled:
			cond = domain.Condition{Type: domain.CondDeliveryHealthy, Status: domain.ConditionUnknown, Reason: "Disabled"}
		case !applied:
			cond.Status, cond.Reason = domain.ConditionUnknown, "NotApplied"
		}
		// A status failure must not fail the reconcile: the next pass rewrites it.
		_ = c.cfg.DestinationStatus.Apply(ctx, engine.Report{
			Ref: engine.NewRef(engine.Kind(StatusKindDestination), dest.ID, d.TeamID), Generation: &gen, Phase: phase,
			Conditions: []domain.Condition{cond}, Replace: true, ReconciledAt: now,
		})
	}
}

// Validate re-checks a destination before it becomes an output. The API validated it on the way in;
// the reconciler also guards the configuration it renders, because a row can be written by anything
// with database access.
func Validate(d Destination) error {
	p := domain.NewLogDestinationParams{Name: d.Name, Kind: d.Kind, Config: d.Config}
	if d.SecretARN != "" {
		// The API stores the secret id; the reconciler reads it as an ARN. Any non-empty value stands in.
		p.SecretID = "sec_00000000000000000000000000"
	}
	if err := p.Validate(); err != nil {
		return err
	}
	for k, v := range d.Config {
		if containsUnsafe(v) {
			return fmt.Errorf("config %s: contains a character that cannot be rendered", k)
		}
	}
	return nil
}

func containsUnsafe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '$' {
			return true
		}
	}
	return false
}
