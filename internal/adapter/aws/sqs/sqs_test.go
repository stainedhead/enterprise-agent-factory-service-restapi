package sqs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
)

const arn = "arn:aws:sqs:us-east-1:111122223333:afs-email.fifo"

type fake struct {
	urls     int
	got      *awssqs.ReceiveMessageInput
	deleted  string
	failURL  bool
	failRecv bool
	failDel  bool
}

func (f *fake) GetQueueUrl(_ context.Context, in *awssqs.GetQueueUrlInput, _ ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error) {
	f.urls++
	if f.failURL {
		return nil, errors.New("nope")
	}
	if aws.ToString(in.QueueName) != "afs-email.fifo" || aws.ToString(in.QueueOwnerAWSAccountId) != "111122223333" {
		return nil, errors.New("wrong queue")
	}
	return &awssqs.GetQueueUrlOutput{QueueUrl: aws.String("https://sqs.example/queue")}, nil
}

func (f *fake) ReceiveMessage(_ context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	f.got = in
	if f.failRecv {
		return nil, errors.New("nope")
	}
	return &awssqs.ReceiveMessageOutput{Messages: []types.Message{{MessageId: aws.String("m1"), ReceiptHandle: aws.String("r1"), Body: aws.String("{}"),
		Attributes:        map[string]string{"ApproximateReceiveCount": "2"},
		MessageAttributes: map[string]types.MessageAttributeValue{"ERROR_CODE": {StringValue: aws.String("X")}}}}}, nil
}

func (f *fake) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	if f.failDel {
		return nil, errors.New("nope")
	}
	f.deleted = aws.ToString(in.ReceiptHandle)
	return &awssqs.DeleteMessageOutput{}, nil
}

func TestQueue(t *testing.T) {
	f := &fake{}
	q, err := sqs.NewWithAPI(f, arn)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Receive(t.Context(), 50)
	if err != nil || len(msgs) != 1 || msgs[0].Attrs["ApproximateReceiveCount"] != "2" || msgs[0].Attrs["ERROR_CODE"] != "X" || f.got.MaxNumberOfMessages != 10 {
		t.Fatalf("%+v %v", msgs, err)
	}
	if err := q.Delete(t.Context(), msgs[0]); err != nil || f.deleted != "r1" || f.urls != 1 {
		t.Fatalf("%v %q urls=%d", err, f.deleted, f.urls)
	}
	f.failRecv, f.failDel = true, true
	if _, err := q.Receive(t.Context(), 1); err == nil {
		t.Fatal("want error")
	}
	if err := q.Delete(t.Context(), msgs[0]); err == nil {
		t.Fatal("want error")
	}
	q2, _ := sqs.NewWithAPI(&fake{failURL: true}, arn)
	if _, err := q2.Receive(t.Context(), 1); err == nil {
		t.Fatal("want url error")
	}
	if err := q2.Delete(t.Context(), msgs[0]); err == nil {
		t.Fatal("want url error")
	}
	for _, bad := range []string{"", "arn:aws:s3:::x", "https://sqs.example/q"} {
		if _, err := sqs.NewWithAPI(f, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
