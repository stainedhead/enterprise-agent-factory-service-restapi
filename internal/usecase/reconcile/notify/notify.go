// Package notify reconciles notification channels (RC-15): each channel row becomes one EventBridge
// rule on the platform event bus, filtered on the channel's team and event types (and on report.*
// events addressed to the channel, OQ-S2), with an API destination and connection for Slack, webhook
// and Jira channels, an SQS target for email (drained here and sent with SES once per event id) or a
// bus target for EventBridge channels. It also runs the delivery side that EventBridge cannot do for
// us: draining the dead-letter queue into the channel status (retries are EventBridge's retry policy),
// sending the test messages TestNotificationChannel asks for (OQ-S1), and sweeping rules of channels
// that no longer exist.
//
// The resource is the channel: engine kind Kind, id = channel id. Credentials are read from Secrets
// Manager in memory, passed to the adapter in a Credential (which prints as [redacted]) and never
// logged, stored or put in an error (OQ-S3).
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Kind is the engine kind (and resource_status kind) of a notification channel.
const Kind engine.Kind = engine.Kind(domain.ChannelStatusKind)

// Condition types.
const (
	// CondRuleReady is True once the channel's rule, target and (if any) connection are applied.
	CondRuleReady = "RuleReady"
)

// Credential is a channel's secret value held in memory. It prints as [redacted] everywhere.
type Credential string

// String implements fmt.Stringer.
func (Credential) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer.
func (Credential) GoString() string { return "notify.Credential([redacted])" }

// LogValue implements slog.LogValuer.
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Reveal returns the value. Only the adapter that hands it to AWS or the receiving service calls it.
func (c Credential) Reveal() string { return string(c) }

// Channel is a channel as the reconciler reads it. SecretARN is the Secrets Manager ARN of the
// referenced team secret ("" for none); the value is never read from the database.
type Channel struct {
	ID         string
	TeamID     string // "" for a service-level channel
	Name       string
	Kind       domain.ChannelKind
	Config     map[string]any
	SecretARN  string
	EventTypes []string
	Enabled    bool
	Version    int64
	// TeamState is the owning team's state (active, suspended, deleting, deleted); "active" for service channels.
	TeamState string
	// Prior are the stored status conditions (delivery history carried across reconciles).
	Prior              []domain.Condition
	ObservedGeneration int64
}

// RuleSpec is what the bus adapter applies for one channel.
type RuleSpec struct {
	Channel Channel
	// Pattern is the rule's event pattern (EventPattern).
	Pattern string
	// Credential is the channel's secret value ("" when it has none).
	Credential Credential
}

// Bus applies and removes a channel's rule and the objects around it. Implementations are idempotent
// and never put a credential in an error.
type Bus interface {
	// Apply creates or updates the rule, its target and (for Slack, webhook and Jira) the connection and
	// API destination. It returns the objects it created or changed.
	Apply(ctx context.Context, spec RuleSpec) ([]string, error)
	// Remove deletes everything Apply made for the channel (targets, rule, API destination, connection).
	Remove(ctx context.Context, channelID string) error
	// List returns the ids of the channels that have a rule.
	List(ctx context.Context) ([]string, error)
}

// Source reads channels.
type Source interface {
	// LoadChannel returns the channel with its team's state, or a domain NotFound error.
	LoadChannel(ctx context.Context, id string) (Channel, error)
}

// Secrets reads a secret value in memory.
type Secrets interface {
	Read(ctx context.Context, arn string) (Credential, error)
}

// StatusCleaner removes the resource_status row of a channel that no longer exists.
type StatusCleaner interface {
	DeleteStatus(ctx context.Context, channelID string) error
}

// Config configures the Controller.
type Config struct {
	Source  Source
	Bus     Bus
	Secrets Secrets
	Cleaner StatusCleaner
	Now     func() time.Time
}

// Controller reconciles Kind.
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Bus == nil || cfg.Secrets == nil {
		return nil, errors.New("notify: Source, Bus and Secrets are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (c *Controller) Kind() engine.Kind { return Kind }

// Route routes changes of notification_channels to the channel. A change with no team (a service-level
// channel) reconciles every channel, which is cheap and idempotent.
func Route() engine.Route {
	return engine.Route{Table: "notification_channels", Targets: []engine.Target{{Kind: Kind, Self: true}}, FanOut: []engine.Kind{Kind}}
}

// needsCredential says whether a kind's rule is built from the secret.
func needsCredential(k domain.ChannelKind) bool {
	return k == domain.ChannelSlack || k == domain.ChannelWebhook || k == domain.ChannelJira
}

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	ch, err := c.cfg.Source.LoadChannel(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return c.gone(ctx, ref.ID)
		}
		return engine.Outcome{}, err
	}
	switch ch.TeamState {
	case "deleted", "deleting":
		// The team's rows go with it; take the rule off the bus now.
		if err := c.cfg.Bus.Remove(ctx, ch.ID); err != nil {
			return engine.Outcome{}, engine.WithCondition(CondRuleReady, err)
		}
		return engine.Outcome{NoStatus: true}, nil
	}
	gen := ch.Version
	now := c.cfg.Now()
	detail, hasDetail := detailCondition(ch.Prior)

	withDetail := func(conds ...domain.Condition) []domain.Condition {
		if hasDetail {
			conds = append(conds, detail)
		}
		return conds
	}

	switch {
	case !ch.Enabled:
		if err := c.cfg.Bus.Remove(ctx, ch.ID); err != nil {
			return engine.Outcome{}, engine.WithCondition(CondRuleReady, err)
		}
		return engine.Outcome{Generation: gen, Conditions: withDetail(
			cond(CondRuleReady, domain.ConditionTrue, "Disabled", "no rule while the channel is disabled"),
			cond(domain.CondDeliveryHealthy, domain.ConditionUnknown, domain.DeliveryDisabled, ""))}, nil
	case ch.TeamID == "" && len(ch.EventTypes) == 0:
		// A service-level channel with no event types is an address book entry (work order email), not a subscription.
		if err := c.cfg.Bus.Remove(ctx, ch.ID); err != nil {
			return engine.Outcome{}, engine.WithCondition(CondRuleReady, err)
		}
		return engine.Outcome{Generation: gen, Conditions: withDetail(
			cond(CondRuleReady, domain.ConditionTrue, "NoRule", "a service-level channel without event types has no rule"),
			cond(domain.CondDeliveryHealthy, domain.ConditionUnknown, domain.DeliveryNoEventTypes, ""))}, nil
	}

	if err := Validate(ch); err != nil {
		out := engine.Outcome{Generation: gen, Conditions: withDetail(
			cond(CondRuleReady, domain.ConditionFalse, domain.DeliveryInvalidChannel, err.Error()),
			cond(domain.CondDeliveryHealthy, domain.ConditionFalse, domain.DeliveryInvalidChannel, err.Error()))}
		return out, engine.Terminal(engine.WithCondition(CondRuleReady, err))
	}
	pattern, err := EventPattern(ch)
	if err != nil {
		return engine.Outcome{}, engine.Terminal(engine.WithCondition(CondRuleReady, err))
	}
	var secret Credential
	if needsCredential(ch.Kind) && ch.SecretARN != "" {
		if secret, err = c.cfg.Secrets.Read(ctx, ch.SecretARN); err != nil {
			return engine.Outcome{}, engine.WithCondition(CondRuleReady, fmt.Errorf("read channel secret: %w", err))
		}
	}
	changed, err := c.cfg.Bus.Apply(ctx, RuleSpec{Channel: ch, Pattern: pattern, Credential: secret})
	if err != nil {
		return engine.Outcome{Changed: changed}, engine.WithCondition(CondRuleReady, err)
	}

	// A config change resets the health the earlier configuration earned; otherwise the history stays.
	health := cond(domain.CondDeliveryHealthy, domain.ConditionTrue, domain.DeliveryRuleApplied, "")
	if h, ok := findCond(ch.Prior, domain.CondDeliveryHealthy); ok && ch.ObservedGeneration == gen && h.Status != domain.ConditionUnknown {
		health = h
	}
	health.LastTransitionTime = now
	return engine.Outcome{Generation: gen, Changed: changed, Conditions: withDetail(
		cond(CondRuleReady, domain.ConditionTrue, "Applied", ""), health)}, nil
}

// gone removes the rule of a channel whose row is gone and its status row.
func (c *Controller) gone(ctx context.Context, id string) (engine.Outcome, error) {
	if err := c.cfg.Bus.Remove(ctx, id); err != nil {
		return engine.Outcome{}, engine.WithCondition(CondRuleReady, err)
	}
	if c.cfg.Cleaner != nil {
		if err := c.cfg.Cleaner.DeleteStatus(ctx, id); err != nil {
			return engine.Outcome{}, err
		}
	}
	return engine.Outcome{NoStatus: true}, nil
}

func cond(typ string, s domain.ConditionStatus, reason, msg string) domain.Condition {
	return domain.Condition{Type: typ, Status: s, Reason: reason, Message: msg}
}

func findCond(cs []domain.Condition, typ string) (domain.Condition, bool) {
	for _, c := range cs {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func detailCondition(cs []domain.Condition) (domain.Condition, bool) {
	return findCond(cs, domain.CondDeliveryDetail)
}

// Validate re-checks a channel before it becomes a rule. The API validated it on the way in; the
// reconciler also guards what it renders, because a row can be written by anything with database access.
func Validate(ch Channel) error {
	p := domain.NewChannelParams{Name: ch.Name, Kind: ch.Kind, Config: ch.Config, EventTypes: ch.EventTypes}
	if ch.SecretARN != "" {
		// The API stores the secret id; the reconciler reads it as an ARN. Any non-empty value stands in.
		p.SecretID = "sec_00000000000000000000000000"
	}
	return p.Validate(ch.TeamID == "")
}

// RedactURLs replaces URLs in an error text with <url>: a webhook URL is a credential (OQ-S3) and must
// not reach a status, an event or a log through a failure message.
func RedactURLs(s string) string {
	var b strings.Builder
	for _, f := range strings.Fields(s) {
		if strings.Contains(f, "://") {
			f = "<url>"
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f)
	}
	out := b.String()
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}
