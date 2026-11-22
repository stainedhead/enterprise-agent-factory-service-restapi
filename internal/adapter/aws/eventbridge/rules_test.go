package eventbridge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eventbridge"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

const (
	chID = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

// fakeAPI is an in-memory EventBridge: rules with targets, connections and API destinations.
type fakeAPI struct {
	rules   map[string]*eb.PutRuleInput
	targets map[string][]types.Target
	conns   map[string]*eb.CreateConnectionInput
	updated map[string]bool
	dests   map[string]*eb.CreateApiDestinationInput
	events  []types.PutEventsRequestEntry
	fail    string // operation name that fails
	failTgt bool
}

func newFake() *fakeAPI {
	return &fakeAPI{rules: map[string]*eb.PutRuleInput{}, targets: map[string][]types.Target{}, conns: map[string]*eb.CreateConnectionInput{},
		dests: map[string]*eb.CreateApiDestinationInput{}, updated: map[string]bool{}}
}

func (f *fakeAPI) err(op string) error {
	if f.fail == op {
		return errors.New("InternalFailure in " + op)
	}
	return nil
}

func nf() error { return &types.ResourceNotFoundException{Message: aws.String("gone")} }

func (f *fakeAPI) PutRule(_ context.Context, in *eb.PutRuleInput, _ ...func(*eb.Options)) (*eb.PutRuleOutput, error) {
	if err := f.err("PutRule"); err != nil {
		return nil, err
	}
	f.rules[*in.Name] = in
	return &eb.PutRuleOutput{}, nil
}

func (f *fakeAPI) PutTargets(_ context.Context, in *eb.PutTargetsInput, _ ...func(*eb.Options)) (*eb.PutTargetsOutput, error) {
	if err := f.err("PutTargets"); err != nil {
		return nil, err
	}
	if f.failTgt {
		return &eb.PutTargetsOutput{FailedEntryCount: 1, FailedEntries: []types.PutTargetsResultEntry{{ErrorCode: aws.String("ValidationException"), ErrorMessage: aws.String("bad https://x.example/y")}}}, nil
	}
	f.targets[*in.Rule] = in.Targets
	return &eb.PutTargetsOutput{}, nil
}

func (f *fakeAPI) RemoveTargets(_ context.Context, in *eb.RemoveTargetsInput, _ ...func(*eb.Options)) (*eb.RemoveTargetsOutput, error) {
	if err := f.err("RemoveTargets"); err != nil {
		return nil, err
	}
	delete(f.targets, *in.Rule)
	return &eb.RemoveTargetsOutput{}, nil
}

func (f *fakeAPI) DeleteRule(_ context.Context, in *eb.DeleteRuleInput, _ ...func(*eb.Options)) (*eb.DeleteRuleOutput, error) {
	if err := f.err("DeleteRule"); err != nil {
		return nil, err
	}
	if _, ok := f.rules[*in.Name]; !ok {
		return nil, nf()
	}
	delete(f.rules, *in.Name)
	return &eb.DeleteRuleOutput{}, nil
}

func (f *fakeAPI) ListRules(_ context.Context, in *eb.ListRulesInput, _ ...func(*eb.Options)) (*eb.ListRulesOutput, error) {
	if err := f.err("ListRules"); err != nil {
		return nil, err
	}
	var out []types.Rule
	for n := range f.rules {
		if strings.HasPrefix(n, *in.NamePrefix) {
			out = append(out, types.Rule{Name: aws.String(n)})
		}
	}
	if in.NextToken == nil && len(out) > 1 { // exercise paging
		return &eb.ListRulesOutput{Rules: out[:1], NextToken: aws.String("t")}, nil
	}
	if in.NextToken != nil {
		return &eb.ListRulesOutput{Rules: out[1:]}, nil
	}
	return &eb.ListRulesOutput{Rules: out}, nil
}

func (f *fakeAPI) ListTargetsByRule(_ context.Context, in *eb.ListTargetsByRuleInput, _ ...func(*eb.Options)) (*eb.ListTargetsByRuleOutput, error) {
	if err := f.err("ListTargetsByRule"); err != nil {
		return nil, err
	}
	if _, ok := f.rules[*in.Rule]; !ok {
		return nil, nf()
	}
	return &eb.ListTargetsByRuleOutput{Targets: f.targets[*in.Rule]}, nil
}

func (f *fakeAPI) DescribeConnection(_ context.Context, in *eb.DescribeConnectionInput, _ ...func(*eb.Options)) (*eb.DescribeConnectionOutput, error) {
	if err := f.err("DescribeConnection"); err != nil {
		return nil, err
	}
	if _, ok := f.conns[*in.Name]; !ok {
		return nil, nf()
	}
	return &eb.DescribeConnectionOutput{ConnectionArn: aws.String("arn:aws:events:us-east-1:111122223333:connection/" + *in.Name + "/1")}, nil
}

func (f *fakeAPI) CreateConnection(_ context.Context, in *eb.CreateConnectionInput, _ ...func(*eb.Options)) (*eb.CreateConnectionOutput, error) {
	if err := f.err("CreateConnection"); err != nil {
		return nil, err
	}
	f.conns[*in.Name] = in
	return &eb.CreateConnectionOutput{ConnectionArn: aws.String("arn:aws:events:us-east-1:111122223333:connection/" + *in.Name + "/1")}, nil
}

func (f *fakeAPI) UpdateConnection(_ context.Context, in *eb.UpdateConnectionInput, _ ...func(*eb.Options)) (*eb.UpdateConnectionOutput, error) {
	if err := f.err("UpdateConnection"); err != nil {
		return nil, err
	}
	f.updated["conn/"+*in.Name] = true
	return &eb.UpdateConnectionOutput{ConnectionArn: aws.String("arn:aws:events:us-east-1:111122223333:connection/" + *in.Name + "/1")}, nil
}

func (f *fakeAPI) DeleteConnection(_ context.Context, in *eb.DeleteConnectionInput, _ ...func(*eb.Options)) (*eb.DeleteConnectionOutput, error) {
	if err := f.err("DeleteConnection"); err != nil {
		return nil, err
	}
	if _, ok := f.conns[*in.Name]; !ok {
		return nil, nf()
	}
	delete(f.conns, *in.Name)
	return &eb.DeleteConnectionOutput{}, nil
}

func (f *fakeAPI) DescribeApiDestination(_ context.Context, in *eb.DescribeApiDestinationInput, _ ...func(*eb.Options)) (*eb.DescribeApiDestinationOutput, error) {
	if err := f.err("DescribeApiDestination"); err != nil {
		return nil, err
	}
	if _, ok := f.dests[*in.Name]; !ok {
		return nil, nf()
	}
	return &eb.DescribeApiDestinationOutput{}, nil
}

func (f *fakeAPI) CreateApiDestination(_ context.Context, in *eb.CreateApiDestinationInput, _ ...func(*eb.Options)) (*eb.CreateApiDestinationOutput, error) {
	if err := f.err("CreateApiDestination"); err != nil {
		return nil, err
	}
	f.dests[*in.Name] = in
	return &eb.CreateApiDestinationOutput{ApiDestinationArn: aws.String("arn:aws:events:us-east-1:111122223333:api-destination/" + *in.Name + "/1")}, nil
}

func (f *fakeAPI) UpdateApiDestination(_ context.Context, in *eb.UpdateApiDestinationInput, _ ...func(*eb.Options)) (*eb.UpdateApiDestinationOutput, error) {
	if err := f.err("UpdateApiDestination"); err != nil {
		return nil, err
	}
	f.updated["dest/"+*in.Name] = true
	f.dests[*in.Name].InvocationEndpoint = in.InvocationEndpoint
	return &eb.UpdateApiDestinationOutput{ApiDestinationArn: aws.String("arn:aws:events:us-east-1:111122223333:api-destination/" + *in.Name + "/1")}, nil
}

func (f *fakeAPI) DeleteApiDestination(_ context.Context, in *eb.DeleteApiDestinationInput, _ ...func(*eb.Options)) (*eb.DeleteApiDestinationOutput, error) {
	if err := f.err("DeleteApiDestination"); err != nil {
		return nil, err
	}
	if _, ok := f.dests[*in.Name]; !ok {
		return nil, nf()
	}
	delete(f.dests, *in.Name)
	return &eb.DeleteApiDestinationOutput{}, nil
}

func (f *fakeAPI) PutEvents(_ context.Context, in *eb.PutEventsInput, _ ...func(*eb.Options)) (*eb.PutEventsOutput, error) {
	if err := f.err("PutEvents"); err != nil {
		return nil, err
	}
	f.events = append(f.events, in.Entries...)
	if f.failTgt {
		return &eb.PutEventsOutput{FailedEntryCount: 1, Entries: []types.PutEventsResultEntry{{ErrorCode: aws.String("AccessDenied"), ErrorMessage: aws.String("no")}}}, nil
	}
	return &eb.PutEventsOutput{Entries: []types.PutEventsResultEntry{{EventId: aws.String("e")}}}, nil
}

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

var cfg = eventbridge.RulesConfig{Bus: "afs-bus", TargetRoleARN: "arn:aws:iam::111122223333:role/afs-notify", EmailQueueARN: "arn:aws:sqs:us-east-1:111122223333:afs-email.fifo", DeadLetterARN: "arn:aws:sqs:us-east-1:111122223333:afs-dlq"}

func spec(kind domain.ChannelKind, c map[string]any, cred string) notify.RuleSpec {
	ch := notify.Channel{ID: chID, TeamID: team, Name: "n", Kind: kind, Config: c, EventTypes: []string{"budget.*"}, Enabled: true, Version: 1}
	p, _ := notify.EventPattern(ch)
	return notify.RuleSpec{Channel: ch, Pattern: p, Credential: notify.Credential(cred)}
}

func TestApplyByKind(t *testing.T) {
	name := notify.ObjectName(chID)
	t.Run("slack", func(t *testing.T) {
		f := newFake()
		r := eventbridge.NewRulesWithAPI(f, cfg)
		changed, err := r.Apply(t.Context(), spec(domain.ChannelSlack, map[string]any{"channel": "#alerts"}, "https://hooks.example.com/T1/B2/X"))
		if err != nil || len(changed) != 1 {
			t.Fatal(changed, err)
		}
		rule := f.rules[name]
		if !strings.Contains(*rule.EventPattern, team) || rule.State != types.RuleStateEnabled || *rule.EventBusName != "afs-bus" {
			t.Fatalf("%+v", rule)
		}
		tg := f.targets[name][0]
		if !strings.Contains(*tg.Arn, "api-destination") || *tg.RoleArn != cfg.TargetRoleARN || *tg.DeadLetterConfig.Arn != cfg.DeadLetterARN ||
			*tg.RetryPolicy.MaximumRetryAttempts != 20 || !strings.Contains(*tg.InputTransformer.InputTemplate, "#alerts") {
			t.Fatalf("%+v", tg)
		}
		if *f.dests[name].InvocationEndpoint != "https://hooks.example.com/T1/B2/X" {
			t.Fatal("slack endpoint is the webhook url")
		}
		// The webhook URL is never the connection's key.
		if v := *f.conns[name].AuthParameters.ApiKeyAuthParameters.ApiKeyValue; strings.Contains(v, "hooks") {
			t.Fatal("credential leaked into the connection marker")
		}
		// Second apply updates in place.
		if _, err := r.Apply(t.Context(), spec(domain.ChannelSlack, map[string]any{"channel": "#alerts"}, "https://hooks.example.com/T1/B2/Y")); err != nil {
			t.Fatal(err)
		}
		if !f.updated["conn/"+name] || !f.updated["dest/"+name] || *f.dests[name].InvocationEndpoint != "https://hooks.example.com/T1/B2/Y" {
			t.Fatalf("%v", f.updated)
		}
	})
	t.Run("slack secret must be https", func(t *testing.T) {
		r := eventbridge.NewRulesWithAPI(newFake(), cfg)
		if _, err := r.Apply(t.Context(), spec(domain.ChannelSlack, map[string]any{"channel": "#alerts"}, "http://x")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("webhook with secret", func(t *testing.T) {
		f := newFake()
		_, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelWebhook, map[string]any{"url": "https://h.example.com/in"}, "Bearer abc"))
		if err != nil || *f.conns[name].AuthParameters.ApiKeyAuthParameters.ApiKeyName != "Authorization" || *f.dests[name].InvocationEndpoint != "https://h.example.com/in" {
			t.Fatal(err)
		}
		if f.targets[name][0].InputTransformer != nil {
			t.Fatal("a webhook gets the raw event")
		}
	})
	t.Run("webhook without secret", func(t *testing.T) {
		f := newFake()
		if _, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelWebhook, map[string]any{"url": "https://h.example.com/in"}, "")); err != nil ||
			*f.conns[name].AuthParameters.ApiKeyAuthParameters.ApiKeyName != "X-Afs-Channel" {
			t.Fatal(err)
		}
	})
	t.Run("jira", func(t *testing.T) {
		f := newFake()
		_, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelJira, map[string]any{"url": "https://j.example.com/", "project": "OPS"}, "me@example.com:tok"))
		if err != nil || *f.conns[name].AuthParameters.BasicAuthParameters.Username != "me@example.com" || *f.dests[name].InvocationEndpoint != "https://j.example.com/rest/api/2/issue" ||
			!strings.Contains(*f.targets[name][0].InputTransformer.InputTemplate, `"key":"OPS"`) {
			t.Fatal(err)
		}
		if _, err := eventbridge.NewRulesWithAPI(newFake(), cfg).Apply(t.Context(), spec(domain.ChannelJira, map[string]any{"url": "https://j.example.com", "project": "OPS"}, "nocolon")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("email goes to the FIFO queue", func(t *testing.T) {
		f := newFake()
		_, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelEmail, map[string]any{"to": []any{"a@example.com"}}, ""))
		tg := f.targets[name][0]
		if err != nil || *tg.Arn != cfg.EmailQueueARN || *tg.SqsParameters.MessageGroupId != chID || tg.DeadLetterConfig != nil || len(f.conns) != 0 {
			t.Fatalf("%v %+v", err, tg)
		}
		if _, err := eventbridge.NewRulesWithAPI(newFake(), eventbridge.RulesConfig{Bus: "b"}).Apply(t.Context(), spec(domain.ChannelEmail, map[string]any{"to": []any{"a@example.com"}}, "")); err == nil {
			t.Fatal("no queue configured must fail")
		}
	})
	t.Run("eventbridge", func(t *testing.T) {
		f := newFake()
		arn := "arn:aws:events:us-east-1:111122223333:event-bus/x"
		if _, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelEventBridge, map[string]any{"eventBusArn": arn}, "")); err != nil || *f.targets[name][0].Arn != arn {
			t.Fatal(err)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		if _, err := eventbridge.NewRulesWithAPI(newFake(), cfg).Apply(t.Context(), spec("sms", nil, "")); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestApplyFailures(t *testing.T) {
	for _, op := range []string{"PutRule", "PutTargets", "DescribeConnection", "CreateConnection", "DescribeApiDestination", "CreateApiDestination"} {
		f := newFake()
		f.fail = op
		_, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelWebhook, map[string]any{"url": "https://h.example.com/in"}, ""))
		if err == nil || !strings.Contains(err.Error(), op) {
			t.Errorf("%s: %v", op, err)
		}
	}
	f := newFake()
	f.failTgt = true
	_, err := eventbridge.NewRulesWithAPI(f, cfg).Apply(t.Context(), spec(domain.ChannelEventBridge, map[string]any{"eventBusArn": "arn:aws:events:us-east-1:111122223333:event-bus/x"}, ""))
	if err == nil || strings.Contains(err.Error(), "x.example") {
		t.Fatalf("rejected target: %v", err)
	}
	// Update paths fail too.
	for _, op := range []string{"UpdateConnection", "UpdateApiDestination"} {
		f := newFake()
		r := eventbridge.NewRulesWithAPI(f, cfg)
		s := spec(domain.ChannelWebhook, map[string]any{"url": "https://h.example.com/in"}, "")
		if _, err := r.Apply(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		f.fail = op
		if _, err := r.Apply(t.Context(), s); err == nil || !strings.Contains(err.Error(), op) {
			t.Errorf("%s: %v", op, err)
		}
	}
}

func TestRemoveListAndTest(t *testing.T) {
	f := newFake()
	r := eventbridge.NewRulesWithAPI(f, cfg)
	other := "nch_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	for _, id := range []string{chID, other} {
		s := spec(domain.ChannelWebhook, map[string]any{"url": "https://h.example.com/in"}, "")
		s.Channel.ID = id
		if _, err := r.Apply(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	f.rules["unrelated-rule"] = &eb.PutRuleInput{}
	ids, err := r.List(t.Context())
	if err != nil || len(ids) != 2 {
		t.Fatalf("%v %v", ids, err)
	}
	if err := r.Remove(t.Context(), chID); err != nil {
		t.Fatal(err)
	}
	name := notify.ObjectName(chID)
	if _, ok := f.rules[name]; ok || len(f.targets[name]) != 0 || f.conns[name] != nil || f.dests[name] != nil {
		t.Fatal("rule, target, connection or destination left behind")
	}
	if len(f.rules) != 2 { // the other channel's rule and the unrelated one stay
		t.Fatalf("%v", f.rules)
	}
	if err := r.Remove(t.Context(), chID); err != nil { // idempotent
		t.Fatal(err)
	}
	for _, op := range []string{"ListTargetsByRule", "RemoveTargets", "DeleteRule", "DeleteApiDestination", "DeleteConnection"} {
		f.fail = op
		if err := r.Remove(t.Context(), other); err == nil || !strings.Contains(err.Error(), op) {
			t.Errorf("%s: %v", op, err)
		}
	}
	f.fail = "ListRules"
	if _, err := r.List(t.Context()); err == nil {
		t.Fatal("want error")
	}
	f.fail = ""
	if err := r.PutTest(t.Context(), "arn:aws:events:us-east-1:111122223333:event-bus/x", notify.TestMessage(notify.Channel{ID: chID}, "ntest_1", t0)); err != nil || len(f.events) != 1 || *f.events[0].Source != "afs" {
		t.Fatal(err, f.events)
	}
	f.failTgt = true
	if err := r.PutTest(t.Context(), "arn", notify.Message{}); err == nil {
		t.Fatal("want rejected")
	}
	f.fail = "PutEvents"
	if err := r.PutTest(t.Context(), "arn", notify.Message{}); err == nil {
		t.Fatal("want error")
	}
}
