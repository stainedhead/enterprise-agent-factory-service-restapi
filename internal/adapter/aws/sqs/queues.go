package sqs

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// QueueAPI is the subset of the SQS SDK client the pool inbox queues use.
type QueueAPI interface {
	CreateQueue(ctx context.Context, in *awssqs.CreateQueueInput, opts ...func(*awssqs.Options)) (*awssqs.CreateQueueOutput, error)
	SetQueueAttributes(ctx context.Context, in *awssqs.SetQueueAttributesInput, opts ...func(*awssqs.Options)) (*awssqs.SetQueueAttributesOutput, error)
	TagQueue(ctx context.Context, in *awssqs.TagQueueInput, opts ...func(*awssqs.Options)) (*awssqs.TagQueueOutput, error)
	GetQueueUrl(ctx context.Context, in *awssqs.GetQueueUrlInput, opts ...func(*awssqs.Options)) (*awssqs.GetQueueUrlOutput, error)
	DeleteQueue(ctx context.Context, in *awssqs.DeleteQueueInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteQueueOutput, error)
}

// Queues implements pool.Queues: one standard queue per pool in the service account.
type Queues struct {
	api                        QueueAPI
	partition, region, account string
}

var _ pool.Queues = (*Queues)(nil)

// NewQueues returns Queues on a configured SDK config for the queues of account in region.
func NewQueues(cfg aws.Config, partition, region, account string) *Queues {
	return NewQueuesWithAPI(awssqs.NewFromConfig(cfg), partition, region, account)
}

// NewQueuesWithAPI returns Queues on api (tests).
func NewQueuesWithAPI(api QueueAPI, partition, region, account string) *Queues {
	return &Queues{api: api, partition: partition, region: region, account: account}
}

// Locate implements pool.Queues: the ARN and URL follow from the name; nothing is called.
func (q *Queues) Locate(name string) pool.Queue {
	return pool.Queue{
		Name: name,
		ARN:  fmt.Sprintf("arn:%s:sqs:%s:%s:%s", q.partition, q.region, q.account, name),
		URL:  fmt.Sprintf("https://sqs.%s.amazonaws.com/%s/%s", q.region, q.account, name),
	}
}

// Ensure implements pool.Queues. CreateQueue is idempotent for the same attributes; the policy and tags are
// then set on every pass, so a changed handoff set or tag takes effect and an unchanged one rewrites nothing
// of substance.
func (q *Queues) Ensure(ctx context.Context, spec pool.QueueSpec) (pool.Queue, error) {
	loc := q.Locate(spec.Name)
	policy, err := InboxPolicy(loc.ARN, spec.ReceiverRoleARN, spec.SenderRoleARNs)
	if err != nil {
		return pool.Queue{}, err
	}
	out, err := q.api.CreateQueue(ctx, &awssqs.CreateQueueInput{QueueName: aws.String(spec.Name), Tags: spec.Tags})
	if err != nil {
		return pool.Queue{}, fmt.Errorf("sqs: CreateQueue %s: %w", spec.Name, err)
	}
	if u := aws.ToString(out.QueueUrl); u != "" {
		loc.URL = u
	}
	if _, err := q.api.SetQueueAttributes(ctx, &awssqs.SetQueueAttributesInput{QueueUrl: aws.String(loc.URL),
		Attributes: map[string]string{"Policy": policy}}); err != nil {
		return pool.Queue{}, fmt.Errorf("sqs: SetQueueAttributes %s: %w", spec.Name, err)
	}
	if len(spec.Tags) > 0 {
		if _, err := q.api.TagQueue(ctx, &awssqs.TagQueueInput{QueueUrl: aws.String(loc.URL), Tags: spec.Tags}); err != nil {
			return pool.Queue{}, fmt.Errorf("sqs: TagQueue %s: %w", spec.Name, err)
		}
	}
	return loc, nil
}

// Delete implements pool.Queues; a queue that does not exist is success.
func (q *Queues) Delete(ctx context.Context, name string) error {
	loc := q.Locate(name)
	_, err := q.api.DeleteQueue(ctx, &awssqs.DeleteQueueInput{QueueUrl: aws.String(loc.URL)})
	if err != nil && !queueMissing(err) {
		return fmt.Errorf("sqs: DeleteQueue %s: %w", name, err)
	}
	return nil
}

func queueMissing(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AWS.SimpleQueueService.NonExistentQueue", "QueueDoesNotExist":
		return true
	}
	return false
}
