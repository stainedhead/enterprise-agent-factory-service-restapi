package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

const (
	recv    = "arn:aws:iam::111122223333:role/afs/ns/afs-pool_01"
	sendA   = "arn:aws:iam::111122223333:role/afs/ns/afs-pool_02"
	sendB   = "arn:aws:iam::111122223333:role/afs/ns/afs-pool_03"
	qName   = "afs-team_01-0a1b2c3d"
	qARN    = "arn:aws:sqs:us-east-1:111122223333:" + qName
	qURL    = "https://sqs.us-east-1.amazonaws.com/111122223333/" + qName
	missing = "AWS.SimpleQueueService.NonExistentQueue"
)

type policyDoc struct {
	Statement []struct {
		Sid       string
		Effect    string
		Principal map[string]any
		Action    []string
		Condition map[string]map[string]any
	}
}

func parse(t *testing.T, s string) policyDoc {
	t.Helper()
	var d policyDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInboxPolicy(t *testing.T) {
	cases := []struct {
		name    string
		senders []string
		sids    []string
	}{
		{"with senders", []string{sendB, sendA, sendA}, []string{"ReceiverAllow", "ReceiverOnly", "SendersAllow", "SendersOnly"}},
		{"no senders closes the queue to every sender", nil, []string{"ReceiverAllow", "ReceiverOnly", "NoSenders"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := InboxPolicy(qARN, recv, c.senders)
			if err != nil {
				t.Fatal(err)
			}
			d := parse(t, p)
			var got []string
			for _, s := range d.Statement {
				got = append(got, s.Sid)
			}
			if strings.Join(got, ",") != strings.Join(c.sids, ",") {
				t.Fatalf("sids = %v, want %v", got, c.sids)
			}
			for _, s := range d.Statement {
				switch s.Sid {
				case "ReceiverOnly":
					if s.Effect != "Deny" || s.Principal["AWS"] != "*" || s.Condition["ArnNotEquals"]["aws:PrincipalArn"] != recv {
						t.Errorf("receiver deny = %+v", s)
					}
				case "ReceiverAllow":
					if s.Effect != "Allow" || s.Principal["AWS"] != "arn:aws:iam::111122223333:root" || s.Condition["ArnEquals"]["aws:PrincipalArn"] != recv {
						t.Errorf("receiver allow = %+v", s)
					}
				case "SendersOnly":
					arns, _ := s.Condition["ArnNotEquals"]["aws:PrincipalArn"].([]any)
					if s.Effect != "Deny" || len(arns) != 2 || arns[0] != sendA || arns[1] != sendB {
						t.Errorf("senders deny = %+v (sorted, deduplicated)", s)
					}
				case "NoSenders":
					if s.Effect != "Deny" || s.Condition != nil || s.Action[0] != "sqs:SendMessage" {
						t.Errorf("no senders = %+v", s)
					}
				}
			}
		})
	}
	a, _ := InboxPolicy(qARN, recv, []string{sendA, sendB})
	b, _ := InboxPolicy(qARN, recv, []string{sendB, sendA})
	if a != b {
		t.Error("the policy must not depend on sender order")
	}
	for _, bad := range []struct{ q, r string }{{"nope", recv}, {qARN, "nope"}, {"arn:aws:s3:::x", recv}} {
		if _, err := InboxPolicy(bad.q, bad.r, nil); err == nil {
			t.Errorf("InboxPolicy(%q,%q) accepted", bad.q, bad.r)
		}
	}
}

type fakeQ struct {
	calls   []string
	policy  string
	tags    map[string]string
	failOn  string
	delErr  error
	created *awssqs.CreateQueueInput
}

func (f *fakeQ) fail(op string) error {
	f.calls = append(f.calls, op)
	if f.failOn == op {
		return errors.New("boom")
	}
	return nil
}
func (f *fakeQ) CreateQueue(_ context.Context, in *awssqs.CreateQueueInput, _ ...func(*awssqs.Options)) (*awssqs.CreateQueueOutput, error) {
	f.created = in
	url := qURL
	return &awssqs.CreateQueueOutput{QueueUrl: &url}, f.fail("Create")
}
func (f *fakeQ) SetQueueAttributes(_ context.Context, in *awssqs.SetQueueAttributesInput, _ ...func(*awssqs.Options)) (*awssqs.SetQueueAttributesOutput, error) {
	f.policy = in.Attributes["Policy"]
	return &awssqs.SetQueueAttributesOutput{}, f.fail("Set")
}
func (f *fakeQ) TagQueue(_ context.Context, in *awssqs.TagQueueInput, _ ...func(*awssqs.Options)) (*awssqs.TagQueueOutput, error) {
	f.tags = in.Tags
	return &awssqs.TagQueueOutput{}, f.fail("Tag")
}
func (f *fakeQ) GetQueueUrl(context.Context, *awssqs.GetQueueUrlInput, ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
	return nil, errors.New("unused")
}
func (f *fakeQ) DeleteQueue(context.Context, *awssqs.DeleteQueueInput, ...func(*awssqs.Options)) (*awssqs.DeleteQueueOutput, error) {
	f.calls = append(f.calls, "Delete")
	return &awssqs.DeleteQueueOutput{}, f.delErr
}

func TestQueuesLocateAndEnsure(t *testing.T) {
	f := &fakeQ{}
	q := NewQueuesWithAPI(f, "aws", "us-east-1", "111122223333")
	if got := q.Locate(qName); got.ARN != qARN || got.URL != qURL || got.Name != qName {
		t.Fatalf("Locate = %+v", got)
	}
	got, err := q.Ensure(t.Context(), pool.QueueSpec{Name: qName, Tags: map[string]string{"team": "x"}, ReceiverRoleARN: recv, SenderRoleARNs: []string{sendA}})
	if err != nil || got.ARN != qARN || got.URL != qURL {
		t.Fatalf("Ensure = %+v, %v", got, err)
	}
	if strings.Join(f.calls, ",") != "Create,Set,Tag" || f.tags["team"] != "x" || !strings.Contains(f.policy, sendA) {
		t.Errorf("calls %v tags %v policy %s", f.calls, f.tags, f.policy)
	}
	for _, op := range []string{"Create", "Set", "Tag"} {
		f := &fakeQ{failOn: op}
		if _, err := NewQueuesWithAPI(f, "aws", "us-east-1", "111122223333").Ensure(t.Context(), pool.QueueSpec{Name: qName, Tags: map[string]string{"a": "b"}, ReceiverRoleARN: recv}); err == nil {
			t.Errorf("%s failure not reported", op)
		}
	}
	if _, err := q.Ensure(t.Context(), pool.QueueSpec{Name: qName, ReceiverRoleARN: "bad"}); err == nil {
		t.Error("a bad receiver ARN must fail before any call")
	}
}

type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestQueuesDelete(t *testing.T) {
	for _, c := range []struct {
		name    string
		err     error
		wantErr bool
	}{{"deleted", nil, false}, {"already gone", apiErr{missing}, false}, {"other error", apiErr{"AccessDenied"}, true}, {"plain error", errors.New("x"), true}} {
		t.Run(c.name, func(t *testing.T) {
			q := NewQueuesWithAPI(&fakeQ{delErr: c.err}, "aws", "us-east-1", "111122223333")
			if err := q.Delete(t.Context(), qName); (err != nil) != c.wantErr {
				t.Errorf("err = %v", err)
			}
		})
	}
}
