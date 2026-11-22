package sqs

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

// IntakeAPI is the subset of the SQS SDK client the intake queue calls.
type IntakeAPI interface {
	ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
}

// intakeVisibility is how long a received message stays hidden. A pass handles at most a hundred messages, one
// database transaction each, so two minutes is ample; a message whose handling failed reappears after it.
const intakeVisibility = 120

// IntakeQueue implements workorder.Queue on the EventBridge intake queue (AFS_WORK_ORDER_QUEUE_URL, M6-E1): the
// queue an EventBridge rule for afs.work_order.requested targets, whose messages are the events themselves.
type IntakeQueue struct {
	api IntakeAPI
	url string
}

var _ workorder.Queue = (*IntakeQueue)(nil)

// NewIntake returns the intake queue at queueURL on a configured SDK config.
func NewIntake(cfg aws.Config, queueURL string) (*IntakeQueue, error) {
	return NewIntakeWithAPI(awssqs.NewFromConfig(cfg), queueURL)
}

// NewIntakeWithAPI returns the intake queue on api (tests).
func NewIntakeWithAPI(api IntakeAPI, queueURL string) (*IntakeQueue, error) {
	u, err := url.Parse(queueURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(u.Path) < 2 {
		return nil, errors.New("sqs: the intake queue URL is not a queue URL")
	}
	return &IntakeQueue{api: api, url: queueURL}, nil
}

// Receive implements workorder.Queue: a short poll of up to max messages (1 to 10).
func (q *IntakeQueue) Receive(ctx context.Context, maxN int) ([]workorder.QueueMessage, error) {
	out, err := q.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl: aws.String(q.url), MaxNumberOfMessages: int32(min(max(maxN, 1), 10)), //nolint:gosec // bounded to 1..10
		VisibilityTimeout: intakeVisibility, WaitTimeSeconds: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("sqs: ReceiveMessage: %w", err)
	}
	msgs := make([]workorder.QueueMessage, 0, len(out.Messages))
	for _, m := range out.Messages {
		msgs = append(msgs, workorder.QueueMessage{ID: aws.ToString(m.MessageId), Receipt: aws.ToString(m.ReceiptHandle), Body: aws.ToString(m.Body)})
	}
	return msgs, nil
}

// Delete implements workorder.Queue.
func (q *IntakeQueue) Delete(ctx context.Context, m workorder.QueueMessage) error {
	if _, err := q.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{QueueUrl: aws.String(q.url), ReceiptHandle: aws.String(m.Receipt)}); err != nil {
		return fmt.Errorf("sqs: DeleteMessage: %w", err)
	}
	return nil
}
