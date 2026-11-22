package sqs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

const intakeURL = "https://sqs.us-east-1.amazonaws.com/111122223333/afs-work-orders"

type intakeFake struct {
	got      *awssqs.ReceiveMessageInput
	deleted  *awssqs.DeleteMessageInput
	failRecv bool
	failDel  bool
}

func (f *intakeFake) ReceiveMessage(_ context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	f.got = in
	if f.failRecv {
		return nil, errors.New("throttled")
	}
	return &awssqs.ReceiveMessageOutput{Messages: []types.Message{
		{MessageId: aws.String("m1"), ReceiptHandle: aws.String("r1"), Body: aws.String(`{"id":"evt-1"}`)},
		{MessageId: aws.String("m2"), ReceiptHandle: aws.String("r2"), Body: aws.String(`{"id":"evt-2"}`)},
	}}, nil
}

func (f *intakeFake) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	f.deleted = in
	if f.failDel {
		return nil, errors.New("expired receipt")
	}
	return &awssqs.DeleteMessageOutput{}, nil
}

// M6-E1: the intake queue receives EventBridge events from the queue URL and deletes handled ones.
func TestIntakeQueue(t *testing.T) {
	f := &intakeFake{}
	q, err := sqs.NewIntakeWithAPI(f, intakeURL)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Receive(t.Context(), 25)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if msgs[0] != (workorder.QueueMessage{ID: "m1", Receipt: "r1", Body: `{"id":"evt-1"}`}) {
		t.Errorf("message = %+v", msgs[0])
	}
	if aws.ToString(f.got.QueueUrl) != intakeURL || f.got.MaxNumberOfMessages != 10 || f.got.VisibilityTimeout != 120 || f.got.WaitTimeSeconds != 0 {
		t.Errorf("receive input = %+v: at most 10 per call, hidden for 2 minutes, short poll", f.got)
	}
	if _, err := q.Receive(t.Context(), 0); err != nil || f.got.MaxNumberOfMessages != 1 {
		t.Errorf("a request for none asks for one: %+v, %v", f.got, err)
	}
	if err := q.Delete(t.Context(), msgs[1]); err != nil || aws.ToString(f.deleted.ReceiptHandle) != "r2" || aws.ToString(f.deleted.QueueUrl) != intakeURL {
		t.Errorf("delete input = %+v, %v", f.deleted, err)
	}

	f.failRecv, f.failDel = true, true
	if _, err := q.Receive(t.Context(), 1); err == nil {
		t.Error("a receive error must be returned")
	}
	if err := q.Delete(t.Context(), msgs[0]); err == nil {
		t.Error("a delete error must be returned")
	}
}

func TestIntakeQueueRefusesANonQueueURL(t *testing.T) {
	for _, u := range []string{"", "afs-work-orders", "arn:aws:sqs:us-east-1:111122223333:afs-work-orders", "https://sqs.example.com", "ftp://sqs.example.com/1/q"} {
		if _, err := sqs.NewIntakeWithAPI(&intakeFake{}, u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
