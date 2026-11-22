package domain

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ChannelKind is where a notification channel delivers (API contract §5).
type ChannelKind string

// Notification channel kinds.
const (
	ChannelSlack       ChannelKind = "slack"
	ChannelEmail       ChannelKind = "email"
	ChannelWebhook     ChannelKind = "webhook"
	ChannelJira        ChannelKind = "jira"
	ChannelEventBridge ChannelKind = "eventbridge"
)

// ChannelKinds lists every kind.
func ChannelKinds() []ChannelKind {
	return []ChannelKind{ChannelSlack, ChannelEmail, ChannelWebhook, ChannelJira, ChannelEventBridge}
}

// Valid reports whether k is a known kind.
func (k ChannelKind) Valid() bool {
	for _, v := range ChannelKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// ChannelStatusKind is the resource_status kind (and engine kind) of a notification channel.
const ChannelStatusKind = "notification_channel"

// CondDeliveryDetail carries the delivery facts a condition has no field for (last delivery, last
// failure, last test) as a JSON message, so the channel status holds them without a schema change.
const CondDeliveryDetail = "DeliveryDetail"

// Delivery reasons of the DeliveryHealthy condition.
const (
	DeliveryRuleApplied    = "RuleApplied"
	DeliveryDelivered      = "Delivered"
	DeliveryFailed         = "DeliveryFailed"
	DeliveryDisabled       = "Disabled"
	DeliveryNoEventTypes   = "NoEventTypes"
	DeliveryTestFailed     = "TestFailed"
	DeliveryInvalidChannel = "InvalidChannel"
)

var (
	channelNameRE  = regexp.MustCompile(`^.{1,100}$`)
	slackChannelRE = regexp.MustCompile(`^(#[a-z0-9][a-z0-9._-]{0,79}|@[a-z0-9][a-z0-9._-]{0,79}|[CGD][A-Z0-9]{8,20})$`)
	jiraProjectRE  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	eventBusARNRE  = regexp.MustCompile(`^arn:aws[a-z-]*:events:[a-z0-9-]+:[0-9]{12}:event-bus/[A-Za-z0-9._-]{1,256}$`)
	eventTypeRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)
	maxEventTypes  = 50
	maxRecipients  = 20
)

type channelKindSpec struct {
	required []string
	optional []string
	secret   SecretPolicy
	check    func(key string, val any) string
}

var channelKindSpecs = map[ChannelKind]channelKindSpec{}

func init() {
	str := func(f func(string) string) func(string, any) string {
		return func(_ string, v any) string {
			s, ok := v.(string)
			if !ok {
				return "must be a string"
			}
			return f(s)
		}
	}
	channelKindSpecs[ChannelSlack] = channelKindSpec{required: []string{"channel"}, optional: []string{"username"}, secret: SecretRequired,
		check: func(k string, v any) string {
			return str(func(s string) string {
				switch k {
				case "channel":
					if !slackChannelRE.MatchString(s) {
						return "must be a Slack channel such as #payments-alerts"
					}
				default:
					if !plainRE.MatchString(s) {
						return "must be 1 to 200 characters of letters, digits and _.:/ -"
					}
				}
				return ""
			})(k, v)
		}}
	channelKindSpecs[ChannelEmail] = channelKindSpec{required: []string{"to"}, optional: []string{"subjectPrefix"}, secret: SecretForbidden,
		check: func(k string, v any) string {
			if k == "subjectPrefix" {
				return str(func(s string) string {
					if len(s) > 100 || strings.ContainsAny(s, "\r\n") {
						return "must be at most 100 characters on one line"
					}
					return ""
				})(k, v)
			}
			return recipientsReason(v)
		}}
	channelKindSpecs[ChannelWebhook] = channelKindSpec{required: []string{"url"}, secret: SecretOptional,
		check: str(endpointReason)}
	channelKindSpecs[ChannelJira] = channelKindSpec{required: []string{"url", "project"}, optional: []string{"issueType"}, secret: SecretRequired,
		check: func(k string, v any) string {
			return str(func(s string) string {
				switch k {
				case "url":
					return endpointReason(s)
				case "project":
					if !jiraProjectRE.MatchString(s) {
						return "must be a Jira project key such as OPS"
					}
				default:
					if !plainRE.MatchString(s) {
						return "must be 1 to 200 characters of letters, digits and _.:/ -"
					}
				}
				return ""
			})(k, v)
		}}
	channelKindSpecs[ChannelEventBridge] = channelKindSpec{required: []string{"eventBusArn"}, secret: SecretForbidden,
		check: str(func(s string) string {
			if !eventBusARNRE.MatchString(s) {
				return "must be an EventBridge event bus ARN"
			}
			return ""
		})}
}

func recipientsReason(v any) string {
	var list []string
	switch t := v.(type) {
	case []string:
		list = t
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return "must be a list of email addresses"
			}
			list = append(list, s)
		}
	default:
		return "must be a list of email addresses"
	}
	if len(list) < 1 || len(list) > maxRecipients {
		return fmt.Sprintf("must hold 1 to %d addresses", maxRecipients)
	}
	for _, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil || a.Address != s || strings.ContainsAny(s, "\r\n<>,;") || len(s) > 254 {
			return "must be plain email addresses such as ops@example.com"
		}
	}
	return ""
}

// Recipients returns the "to" addresses of an email channel configuration.
func Recipients(cfg map[string]any) []string {
	var out []string
	switch t := cfg["to"].(type) {
	case []string:
		out = append(out, t...)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// ConfigString returns a string member of a channel configuration ("" when absent).
func ConfigString(cfg map[string]any, key string) string {
	s, _ := cfg[key].(string)
	return s
}

// ValidEventTypeFilter checks one entry of a channel's eventTypes: a known event type, or a prefix
// such as budget.* that matches at least one.
func ValidEventTypeFilter(f string) bool {
	if p, ok := strings.CutSuffix(f, ".*"); ok {
		if !eventTypeRE.MatchString(p) {
			return false
		}
		for _, t := range allEventTypes() {
			if strings.HasPrefix(string(t), p+".") {
				return true
			}
		}
		return false
	}
	return eventTypeRE.MatchString(f) && EventType(f).Known()
}

func allEventTypes() []EventType {
	out := EventTypes()
	for t := range ExtensionEventEmitters {
		out = append(out, t)
	}
	return out
}

// NotificationChannel is a team's (or, with an empty TeamID, the service's) notification target.
// Config holds non-secret settings only; SecretID references the credential. The table has no
// generation column, so Generation is the row version.
type NotificationChannel struct {
	ID         string
	TeamID     string
	Name       string
	Kind       ChannelKind
	Config     map[string]any
	SecretID   string
	EventTypes []string
	Enabled    bool
	Version    Version
	Generation Generation
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
	Status     Status
}

// ServiceLevel reports whether the channel belongs to the service, not a team.
func (c NotificationChannel) ServiceLevel() bool { return c.TeamID == "" }

// NewChannelParams is the input of CreateNotificationChannel and CreateServiceNotificationChannel.
type NewChannelParams struct {
	Name       string
	Kind       ChannelKind
	Config     map[string]any
	SecretID   string
	EventTypes []string
	Enabled    bool
}

// Validate checks the channel as a whole. serviceLevel channels hold no secret (database contract).
func (p NewChannelParams) Validate(serviceLevel bool) error {
	return validateChannel(p.Name, p.Kind, p.Config, p.SecretID, p.EventTypes, serviceLevel)
}

// New builds the channel created by p; teamID is "" for a service-level channel.
func (p NewChannelParams) New(id, teamID string, now time.Time) (NotificationChannel, error) {
	if err := p.Validate(teamID == ""); err != nil {
		return NotificationChannel{}, err
	}
	return NotificationChannel{ID: id, TeamID: teamID, Name: p.Name, Kind: p.Kind, Config: cloneAnyMap(p.Config), SecretID: p.SecretID,
		EventTypes: cloneStrings(p.EventTypes), Enabled: p.Enabled, Version: 1, Generation: 1,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Status: PendingStatus(now)}, nil
}

// ChannelPatch is the input of the update operations; nil means unchanged. The kind cannot change.
// Config and EventTypes replace the whole value. SecretID may be cleared (ClearSecret).
type ChannelPatch struct {
	Name        *string
	Config      map[string]any
	SecretID    *string
	ClearSecret bool
	EventTypes  *[]string
	Enabled     *bool
}

// Apply returns c changed by the patch, validated as a whole.
func (c NotificationChannel) Apply(p ChannelPatch, now time.Time) (NotificationChannel, error) {
	n := c
	n.Config = cloneAnyMap(c.Config)
	n.EventTypes = cloneStrings(c.EventTypes)
	if p.Name != nil {
		n.Name = *p.Name
	}
	if p.Config != nil {
		n.Config = cloneAnyMap(p.Config)
	}
	switch {
	case p.ClearSecret:
		n.SecretID = ""
	case p.SecretID != nil:
		n.SecretID = *p.SecretID
	}
	if p.EventTypes != nil {
		n.EventTypes = cloneStrings(*p.EventTypes)
	}
	if p.Enabled != nil {
		n.Enabled = *p.Enabled
	}
	if err := validateChannel(n.Name, n.Kind, n.Config, n.SecretID, n.EventTypes, n.ServiceLevel()); err != nil {
		return NotificationChannel{}, err
	}
	n.UpdatedAt = now.UTC()
	return n, nil
}

func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func validateChannel(name string, kind ChannelKind, cfg map[string]any, secretID string, eventTypes []string, serviceLevel bool) error {
	var fe []FieldError
	bad := func(f, r string) { fe = append(fe, FieldError{Field: f, Reason: r}) }
	if !channelNameRE.MatchString(name) || strings.TrimSpace(name) == "" {
		bad("name", "must be 1 to 100 characters")
	}
	if secretID != "" {
		if _, err := ParseID(PrefixSecret, secretID); err != nil {
			bad("secretId", "must be a secret id (sec_…)")
		}
	}
	if len(eventTypes) > maxEventTypes {
		bad("eventTypes", fmt.Sprintf("has more than %d entries", maxEventTypes))
	}
	seen := map[string]bool{}
	for i, f := range eventTypes {
		switch {
		case !ValidEventTypeFilter(f):
			bad(fmt.Sprintf("eventTypes[%d]", i), "must be an event type or a prefix such as budget.*")
		case seen[f]:
			bad(fmt.Sprintf("eventTypes[%d]", i), "is listed twice")
		}
		seen[f] = true
	}
	spec, ok := channelKindSpecs[kind]
	if !ok {
		bad("kind", "must be one of slack, email, webhook, jira, eventbridge")
		return Validation(fe...)
	}
	if len(cfg) > maxConfigVals {
		bad("config", "has too many entries")
	}
	allowed := map[string]bool{}
	for _, k := range spec.required {
		allowed[k] = true
	}
	for _, k := range spec.optional {
		allowed[k] = true
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case InlineCredentialKey(k):
			bad("config."+k, "credentials are never stored in a channel; put the value in a secret and pass secretId")
		case !allowed[k]:
			bad("config."+k, "is not a setting of a "+string(kind)+" channel")
		default:
			if r := spec.check(k, cfg[k]); r != "" {
				bad("config."+k, r)
			}
		}
	}
	for _, k := range spec.required {
		if _, ok := cfg[k]; !ok {
			bad("config."+k, "is required for a "+string(kind)+" channel")
		}
	}
	switch {
	case serviceLevel && secretID != "":
		bad("secretId", "service-level channels hold no secret")
	case serviceLevel && spec.secret == SecretRequired:
		bad("kind", string(kind)+" channels need a secret, which a service-level channel cannot hold; use email, webhook or eventbridge")
	case spec.secret == SecretForbidden && secretID != "":
		bad("secretId", string(kind)+" channels take no secret")
	case !serviceLevel && spec.secret == SecretRequired && secretID == "":
		bad("secretId", string(kind)+" channels need a secret holding the credential")
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// DeliveryDetail is the delivery history the reconciler keeps on a channel's status (the
// DeliveryDetail condition's message).
type DeliveryDetail struct {
	LastDeliveredAt *time.Time `json:"lastDeliveredAt,omitempty"`
	LastFailureAt   *time.Time `json:"lastFailureAt,omitempty"`
	LastError       string     `json:"lastError,omitempty"`
	LastTestID      string     `json:"lastTestId,omitempty"`
	TestDelivered   *bool      `json:"testDelivered,omitempty"`
	TestError       string     `json:"testError,omitempty"`
}

// Condition renders the detail as the DeliveryDetail condition.
func (d DeliveryDetail) Condition(now time.Time) Condition {
	b, _ := json.Marshal(d) //nolint:errchkjson // a struct of times and strings cannot fail
	return Condition{Type: CondDeliveryDetail, Status: ConditionTrue, Reason: "Recorded", Message: string(b), LastTransitionTime: now.UTC()}
}

// DeliveryDetailOf reads the detail from conditions (zero when absent or unreadable).
func DeliveryDetailOf(conds []Condition) DeliveryDetail {
	for _, c := range conds {
		if c.Type == CondDeliveryDetail {
			var d DeliveryDetail
			if json.Unmarshal([]byte(c.Message), &d) == nil {
				return d
			}
		}
	}
	return DeliveryDetail{}
}

// DeliveryStatus is the API's view of a channel's delivery health (contract: DeliveryStatus).
type DeliveryStatus struct {
	Healthy         *bool
	LastDeliveredAt *time.Time
	LastError       string
	LastFailureAt   *time.Time
	LastTestID      string
}

// Delivery derives the delivery status from the channel's conditions. Healthy is nil until the
// reconciler has reported (or while the channel is disabled).
func (c NotificationChannel) Delivery() DeliveryStatus {
	d := DeliveryDetailOf(c.Status.Conditions)
	out := DeliveryStatus{LastDeliveredAt: d.LastDeliveredAt, LastError: d.LastError, LastFailureAt: d.LastFailureAt, LastTestID: d.LastTestID}
	if h, ok := c.Status.Condition(CondDeliveryHealthy); ok {
		switch h.Status {
		case ConditionTrue:
			t := true
			out.Healthy = &t
		case ConditionFalse:
			f := false
			out.Healthy = &f
		}
	}
	return out
}

// TestResult is the outcome of a channel test (contract: NotificationTestResult). Pending is true while
// the reconciler has not reported.
type TestResult struct {
	TestID    string
	Delivered bool
	Error     string
	Pending   bool
}
