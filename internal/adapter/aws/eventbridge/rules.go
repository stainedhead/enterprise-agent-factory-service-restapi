package eventbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// RulesAPI is the subset of the EventBridge SDK client the notification rules use; *eb.Client
// satisfies it and tests inject a fake.
type RulesAPI interface {
	PutRule(ctx context.Context, in *eb.PutRuleInput, opts ...func(*eb.Options)) (*eb.PutRuleOutput, error)
	PutTargets(ctx context.Context, in *eb.PutTargetsInput, opts ...func(*eb.Options)) (*eb.PutTargetsOutput, error)
	RemoveTargets(ctx context.Context, in *eb.RemoveTargetsInput, opts ...func(*eb.Options)) (*eb.RemoveTargetsOutput, error)
	DeleteRule(ctx context.Context, in *eb.DeleteRuleInput, opts ...func(*eb.Options)) (*eb.DeleteRuleOutput, error)
	ListRules(ctx context.Context, in *eb.ListRulesInput, opts ...func(*eb.Options)) (*eb.ListRulesOutput, error)
	ListTargetsByRule(ctx context.Context, in *eb.ListTargetsByRuleInput, opts ...func(*eb.Options)) (*eb.ListTargetsByRuleOutput, error)
	DescribeConnection(ctx context.Context, in *eb.DescribeConnectionInput, opts ...func(*eb.Options)) (*eb.DescribeConnectionOutput, error)
	CreateConnection(ctx context.Context, in *eb.CreateConnectionInput, opts ...func(*eb.Options)) (*eb.CreateConnectionOutput, error)
	UpdateConnection(ctx context.Context, in *eb.UpdateConnectionInput, opts ...func(*eb.Options)) (*eb.UpdateConnectionOutput, error)
	DeleteConnection(ctx context.Context, in *eb.DeleteConnectionInput, opts ...func(*eb.Options)) (*eb.DeleteConnectionOutput, error)
	DescribeApiDestination(ctx context.Context, in *eb.DescribeApiDestinationInput, opts ...func(*eb.Options)) (*eb.DescribeApiDestinationOutput, error)
	CreateApiDestination(ctx context.Context, in *eb.CreateApiDestinationInput, opts ...func(*eb.Options)) (*eb.CreateApiDestinationOutput, error)
	UpdateApiDestination(ctx context.Context, in *eb.UpdateApiDestinationInput, opts ...func(*eb.Options)) (*eb.UpdateApiDestinationOutput, error)
	DeleteApiDestination(ctx context.Context, in *eb.DeleteApiDestinationInput, opts ...func(*eb.Options)) (*eb.DeleteApiDestinationOutput, error)
	PutEvents(ctx context.Context, in *eb.PutEventsInput, opts ...func(*eb.Options)) (*eb.PutEventsOutput, error)
}

// RulesConfig configures Rules.
type RulesConfig struct {
	// Bus is the platform event bus the rules live on.
	Bus string
	// TargetRoleARN is the role EventBridge assumes to invoke API destinations and bus targets.
	TargetRoleARN string
	// EmailQueueARN is the FIFO queue email channels target; DeadLetterARN receives events whose
	// delivery failed after the retry policy.
	EmailQueueARN string
	DeadLetterARN string
}

// Rules implements notify.Bus and notify.BusTester on EventBridge.
type Rules struct {
	api RulesAPI
	cfg RulesConfig
}

var (
	_ notify.Bus       = (*Rules)(nil)
	_ notify.BusTester = (*Rules)(nil)
)

// NewRules returns Rules on a configured SDK config.
func NewRules(cfg aws.Config, c RulesConfig) *Rules {
	return &Rules{api: eb.NewFromConfig(cfg), cfg: c}
}

// NewRulesWithAPI returns Rules on api (tests).
func NewRulesWithAPI(api RulesAPI, c RulesConfig) *Rules { return &Rules{api: api, cfg: c} }

// Retry policy of every target: EventBridge retries with backoff for up to two hours, then dead-letters.
const (
	maxRetryAttempts = 20
	maxEventAge      = 2 * time.Hour
	targetID         = "afs"
	// connectionHeader names the placeholder API key of a connection that has no credential of its own.
	connectionHeader = "X-Afs-Channel"
)

func isNotFound(err error) bool {
	var nf *types.ResourceNotFoundException
	return errors.As(err, &nf)
}

func wrap(op string, err error) error { return fmt.Errorf("eventbridge: %s: %w", op, err) }

// Apply implements notify.Bus.
func (r *Rules) Apply(ctx context.Context, s notify.RuleSpec) ([]string, error) {
	ch := s.Channel
	name := notify.ObjectName(ch.ID)
	target, err := r.target(ctx, s, name)
	if err != nil {
		return nil, err
	}
	if _, err := r.api.PutRule(ctx, &eb.PutRuleInput{Name: aws.String(name), EventBusName: aws.String(r.cfg.Bus),
		EventPattern: aws.String(s.Pattern), State: types.RuleStateEnabled, Description: aws.String("AFS notification channel " + ch.ID)}); err != nil {
		return nil, wrap("PutRule", err)
	}
	out, err := r.api.PutTargets(ctx, &eb.PutTargetsInput{Rule: aws.String(name), EventBusName: aws.String(r.cfg.Bus), Targets: []types.Target{target}})
	if err != nil {
		return nil, wrap("PutTargets", err)
	}
	if out.FailedEntryCount > 0 {
		code, msg := "", ""
		if len(out.FailedEntries) > 0 {
			code, msg = aws.ToString(out.FailedEntries[0].ErrorCode), aws.ToString(out.FailedEntries[0].ErrorMessage)
		}
		return nil, fmt.Errorf("eventbridge: PutTargets rejected the target: %s: %s", code, notify.RedactURLs(msg))
	}
	// A previous kind's target id is "afs" too, so a rule never keeps a stale target.
	return []string{"eventbridge-rule/" + name}, nil
}

// target builds the rule's target, making the connection and API destination first where the kind needs them.
func (r *Rules) target(ctx context.Context, s notify.RuleSpec, name string) (types.Target, error) {
	ch := s.Channel
	t := types.Target{Id: aws.String(targetID)}
	switch ch.Kind {
	case domain.ChannelEmail:
		if r.cfg.EmailQueueARN == "" {
			return t, errors.New("eventbridge: the email queue is not configured (AFS_NOTIFY_EMAIL_QUEUE_ARN)")
		}
		t.Arn = aws.String(r.cfg.EmailQueueARN)
		t.SqsParameters = &types.SqsParameters{MessageGroupId: aws.String(ch.ID)}
		t.InputTransformer = &types.InputTransformer{InputPathsMap: eventPaths(),
			InputTemplate: aws.String(`{"channelId":"` + ch.ID + `","eventId":"<id>","detailType":"<dt>","detail":<detail>}`)}
		return t, nil
	case domain.ChannelEventBridge:
		t.Arn = aws.String(domain.ConfigString(ch.Config, "eventBusArn"))
		t.RoleArn = aws.String(r.cfg.TargetRoleARN)
	case domain.ChannelSlack, domain.ChannelWebhook, domain.ChannelJira:
		arn, err := r.apiDestination(ctx, s, name)
		if err != nil {
			return t, err
		}
		t.Arn, t.RoleArn = aws.String(arn), aws.String(r.cfg.TargetRoleARN)
		switch ch.Kind {
		case domain.ChannelSlack:
			t.InputTransformer = &types.InputTransformer{InputPathsMap: eventPaths(),
				InputTemplate: aws.String(`{"channel":"` + domain.ConfigString(ch.Config, "channel") + `","text":"AFS <dt> (team <team>) at <time> [event <id>]"}`)}
		case domain.ChannelJira:
			issue := domain.ConfigString(ch.Config, "issueType")
			if issue == "" {
				issue = "Task"
			}
			t.InputTransformer = &types.InputTransformer{InputPathsMap: eventPaths(),
				InputTemplate: aws.String(`{"fields":{"project":{"key":"` + domain.ConfigString(ch.Config, "project") + `"},"summary":"AFS <dt> (team <team>)","description":"Event <id> at <time>","issuetype":{"name":"` + issue + `"}}}`)}
		}
	default:
		return t, fmt.Errorf("eventbridge: unknown channel kind %q", ch.Kind)
	}
	if r.cfg.DeadLetterARN != "" {
		t.DeadLetterConfig = &types.DeadLetterConfig{Arn: aws.String(r.cfg.DeadLetterARN)}
	}
	t.RetryPolicy = &types.RetryPolicy{MaximumRetryAttempts: aws.Int32(maxRetryAttempts), MaximumEventAgeInSeconds: aws.Int32(int32(maxEventAge / time.Second))}
	return t, nil
}

func eventPaths() map[string]string {
	return map[string]string{"dt": "$.detail-type", "id": "$.id", "team": "$.detail.teamId", "time": "$.time", "detail": "$.detail"}
}

// endpoint is the URL the API destination invokes. A Slack webhook URL is the credential itself and is
// handed to EventBridge, which holds it (OQ-S3); it is never stored by AFS.
func endpoint(s notify.RuleSpec) (string, error) {
	var u string
	switch s.Channel.Kind {
	case domain.ChannelSlack:
		u = s.Credential.Reveal()
		if !strings.HasPrefix(u, "https://") {
			return "", errors.New("the Slack secret must hold an https webhook URL")
		}
	case domain.ChannelWebhook:
		u = domain.ConfigString(s.Channel.Config, "url")
	case domain.ChannelJira:
		u = strings.TrimSuffix(domain.ConfigString(s.Channel.Config, "url"), "/") + "/rest/api/2/issue"
	}
	return u, nil
}

func authParams(s notify.RuleSpec) (types.CreateConnectionAuthRequestParameters, types.UpdateConnectionAuthRequestParameters, types.ConnectionAuthorizationType, error) {
	var (
		create types.CreateConnectionAuthRequestParameters
		update types.UpdateConnectionAuthRequestParameters
	)
	switch {
	case s.Channel.Kind == domain.ChannelJira:
		user, pass, ok := strings.Cut(s.Credential.Reveal(), ":")
		if !ok || user == "" || pass == "" {
			return create, update, "", errors.New("the Jira secret must hold user:api-token")
		}
		create.BasicAuthParameters = &types.CreateConnectionBasicAuthRequestParameters{Username: aws.String(user), Password: aws.String(pass)}
		update.BasicAuthParameters = &types.UpdateConnectionBasicAuthRequestParameters{Username: aws.String(user), Password: aws.String(pass)}
		return create, update, types.ConnectionAuthorizationTypeBasic, nil
	case s.Channel.Kind == domain.ChannelWebhook && s.Credential != "":
		create.ApiKeyAuthParameters = &types.CreateConnectionApiKeyAuthRequestParameters{ApiKeyName: aws.String("Authorization"), ApiKeyValue: aws.String(s.Credential.Reveal())}
		update.ApiKeyAuthParameters = &types.UpdateConnectionApiKeyAuthRequestParameters{ApiKeyName: aws.String("Authorization"), ApiKeyValue: aws.String(s.Credential.Reveal())}
	default:
		// Slack carries its credential in the webhook URL; a webhook without a secret has none. A connection
		// needs some authorization, so it holds a harmless marker header.
		create.ApiKeyAuthParameters = &types.CreateConnectionApiKeyAuthRequestParameters{ApiKeyName: aws.String(connectionHeader), ApiKeyValue: aws.String(s.Channel.ID)}
		update.ApiKeyAuthParameters = &types.UpdateConnectionApiKeyAuthRequestParameters{ApiKeyName: aws.String(connectionHeader), ApiKeyValue: aws.String(s.Channel.ID)}
	}
	return create, update, types.ConnectionAuthorizationTypeApiKey, nil
}

// apiDestination makes or updates the channel's connection and API destination and returns the
// destination's ARN.
func (r *Rules) apiDestination(ctx context.Context, s notify.RuleSpec, name string) (string, error) {
	url, err := endpoint(s)
	if err != nil {
		return "", err
	}
	create, update, typ, err := authParams(s)
	if err != nil {
		return "", err
	}
	var connARN string
	d, err := r.api.DescribeConnection(ctx, &eb.DescribeConnectionInput{Name: aws.String(name)})
	switch {
	case err == nil:
		out, err := r.api.UpdateConnection(ctx, &eb.UpdateConnectionInput{Name: aws.String(name), AuthorizationType: typ, AuthParameters: &update})
		if err != nil {
			return "", wrap("UpdateConnection", err)
		}
		connARN = aws.ToString(out.ConnectionArn)
		if connARN == "" {
			connARN = aws.ToString(d.ConnectionArn)
		}
	case isNotFound(err):
		out, err := r.api.CreateConnection(ctx, &eb.CreateConnectionInput{Name: aws.String(name), AuthorizationType: typ, AuthParameters: &create})
		if err != nil {
			return "", wrap("CreateConnection", err)
		}
		connARN = aws.ToString(out.ConnectionArn)
	default:
		return "", wrap("DescribeConnection", err)
	}
	_, err = r.api.DescribeApiDestination(ctx, &eb.DescribeApiDestinationInput{Name: aws.String(name)})
	switch {
	case err == nil:
		out, err := r.api.UpdateApiDestination(ctx, &eb.UpdateApiDestinationInput{Name: aws.String(name), ConnectionArn: aws.String(connARN),
			InvocationEndpoint: aws.String(url), HttpMethod: types.ApiDestinationHttpMethodPost})
		if err != nil {
			return "", wrap("UpdateApiDestination", err)
		}
		return aws.ToString(out.ApiDestinationArn), nil
	case isNotFound(err):
		out, err := r.api.CreateApiDestination(ctx, &eb.CreateApiDestinationInput{Name: aws.String(name), ConnectionArn: aws.String(connARN),
			InvocationEndpoint: aws.String(url), HttpMethod: types.ApiDestinationHttpMethodPost, InvocationRateLimitPerSecond: aws.Int32(5)})
		if err != nil {
			return "", wrap("CreateApiDestination", err)
		}
		return aws.ToString(out.ApiDestinationArn), nil
	}
	return "", wrap("DescribeApiDestination", err)
}

// Remove implements notify.Bus: targets, rule, API destination and connection, each tolerating that it is gone.
func (r *Rules) Remove(ctx context.Context, channelID string) error {
	name := notify.ObjectName(channelID)
	bus := aws.String(r.cfg.Bus)
	ts, err := r.api.ListTargetsByRule(ctx, &eb.ListTargetsByRuleInput{Rule: aws.String(name), EventBusName: bus})
	switch {
	case err == nil:
		if len(ts.Targets) > 0 {
			ids := make([]string, 0, len(ts.Targets))
			for _, t := range ts.Targets {
				ids = append(ids, aws.ToString(t.Id))
			}
			if _, err := r.api.RemoveTargets(ctx, &eb.RemoveTargetsInput{Rule: aws.String(name), EventBusName: bus, Ids: ids, Force: true}); err != nil && !isNotFound(err) {
				return wrap("RemoveTargets", err)
			}
		}
		if _, err := r.api.DeleteRule(ctx, &eb.DeleteRuleInput{Name: aws.String(name), EventBusName: bus, Force: true}); err != nil && !isNotFound(err) {
			return wrap("DeleteRule", err)
		}
	case !isNotFound(err):
		return wrap("ListTargetsByRule", err)
	}
	if _, err := r.api.DeleteApiDestination(ctx, &eb.DeleteApiDestinationInput{Name: aws.String(name)}); err != nil && !isNotFound(err) {
		return wrap("DeleteApiDestination", err)
	}
	if _, err := r.api.DeleteConnection(ctx, &eb.DeleteConnectionInput{Name: aws.String(name)}); err != nil && !isNotFound(err) {
		return wrap("DeleteConnection", err)
	}
	return nil
}

// List implements notify.Bus.
func (r *Rules) List(ctx context.Context) ([]string, error) {
	var ids []string
	var token *string
	for {
		out, err := r.api.ListRules(ctx, &eb.ListRulesInput{EventBusName: aws.String(r.cfg.Bus), NamePrefix: aws.String(notify.RulePrefix + string(domain.PrefixChannel) + "_"), NextToken: token})
		if err != nil {
			return nil, wrap("ListRules", err)
		}
		for _, rule := range out.Rules {
			if id, ok := notify.ChannelIDOf(aws.ToString(rule.Name)); ok {
				ids = append(ids, id)
			}
		}
		if out.NextToken == nil {
			return ids, nil
		}
		token = out.NextToken
	}
}

// PutTest implements notify.BusTester: one test event on the channel's target bus.
func (r *Rules) PutTest(ctx context.Context, busARN string, m notify.Message) error {
	out, err := r.api.PutEvents(ctx, &eb.PutEventsInput{Entries: []types.PutEventsRequestEntry{{
		EventBusName: aws.String(busARN), Source: aws.String(domain.EventSource), DetailType: aws.String(m.EventType), Detail: aws.String(string(m.Detail))}}})
	if err != nil {
		return wrap("PutEvents", err)
	}
	if out.FailedEntryCount > 0 && len(out.Entries) > 0 {
		return fmt.Errorf("eventbridge: PutEvents rejected the test event: %s: %s", aws.ToString(out.Entries[0].ErrorCode), notify.RedactURLs(aws.ToString(out.Entries[0].ErrorMessage)))
	}
	return nil
}
