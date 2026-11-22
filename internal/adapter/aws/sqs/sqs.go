// Package sqs is the SQS adapter of the reconciler's notification delivery: it receives and deletes
// the messages of the email queue and of the EventBridge dead-letter queue.
package sqs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// API is the subset of the SQS SDK client this package calls.
type API interface {
	GetQueueUrl(ctx context.Context, in *awssqs.GetQueueUrlInput, opts ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error)
	ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
}

// Queue implements notify.Queue on one queue named by its ARN.
type Queue struct {
	api API
	arn string
	url string
}

var _ notify.Queue = (*Queue)(nil)

// New returns a Queue on a configured SDK config. arn is the queue's ARN (arn:aws:sqs:region:account:name).
func New(cfg aws.Config, arn string) (*Queue, error) {
	return NewWithAPI(awssqs.NewFromConfig(cfg), arn)
}

// NewWithAPI returns a Queue on api (tests).
func NewWithAPI(api API, arn string) (*Queue, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[5] == "" {
		return nil, errors.New("sqs: not an SQS queue ARN")
	}
	return &Queue{api: api, arn: arn}, nil
}

func (q *Queue) queueURL(ctx context.Context) (string, error) {
	if q.url != "" {
		return q.url, nil
	}
	parts := strings.Split(q.arn, ":")
	out, err := q.api.GetQueueUrl(ctx, &awssqs.GetQueueUrlInput{QueueName: aws.String(parts[5]), QueueOwnerAWSAccountId: aws.String(parts[4])})
	if err != nil {
		return "", fmt.Errorf("sqs: GetQueueUrl: %w", err)
	}
	q.url = aws.ToString(out.QueueUrl)
	return q.url, nil
}

// Receive implements notify.Queue: a short poll of up to max messages, with their system and message attributes.
func (q *Queue) Receive(ctx context.Context, maxN int) ([]notify.QueueMessage, error) {
	url, err := q.queueURL(ctx)
	if err != nil {
		return nil, err
	}
	out, err := q.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{QueueUrl: aws.String(url), MaxNumberOfMessages: int32(min(max(maxN, 1), 10)), //nolint:gosec // bounded to 1..10
		VisibilityTimeout: 120, WaitTimeSeconds: 0,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
		MessageAttributeNames:       []string{"All"}})
	if err != nil {
		return nil, fmt.Errorf("sqs: ReceiveMessage: %w", err)
	}
	msgs := make([]notify.QueueMessage, 0, len(out.Messages))
	for _, m := range out.Messages {
		attrs := map[string]string{}
		for k, v := range m.MessageAttributes {
			attrs[k] = aws.ToString(v.StringValue)
		}
		for k, v := range m.Attributes {
			attrs[k] = v
		}
		msgs = append(msgs, notify.QueueMessage{ID: aws.ToString(m.MessageId), Receipt: aws.ToString(m.ReceiptHandle), Body: aws.ToString(m.Body), Attrs: attrs})
	}
	return msgs, nil
}

// Delete implements notify.Queue.
func (q *Queue) Delete(ctx context.Context, m notify.QueueMessage) error {
	url, err := q.queueURL(ctx)
	if err != nil {
		return err
	}
	if _, err := q.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: aws.String(m.Receipt)}); err != nil {
		return fmt.Errorf("sqs: DeleteMessage: %w", err)
	}
	return nil
}
