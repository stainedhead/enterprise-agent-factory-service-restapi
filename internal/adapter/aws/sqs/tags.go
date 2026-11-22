package sqs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

// TagAPI is the subset of the SQS SDK client the tag sync calls.
type TagAPI interface {
	ListQueueTags(ctx context.Context, in *awssqs.ListQueueTagsInput, opts ...func(*awssqs.Options)) (*awssqs.ListQueueTagsOutput, error)
	TagQueue(ctx context.Context, in *awssqs.TagQueueInput, opts ...func(*awssqs.Options)) (*awssqs.TagQueueOutput, error)
	UntagQueue(ctx context.Context, in *awssqs.UntagQueueInput, opts ...func(*awssqs.Options)) (*awssqs.UntagQueueOutput, error)
}

// QueueTagger implements tagsync.Tagger for queues addressed by URL.
type QueueTagger struct{ api TagAPI }

var _ tagsync.Tagger = (*QueueTagger)(nil)

// NewQueueTagger returns a QueueTagger on a configured SDK config.
func NewQueueTagger(cfg aws.Config) *QueueTagger { return &QueueTagger{api: awssqs.NewFromConfig(cfg)} }

// NewQueueTaggerWithAPI returns a QueueTagger on api (tests).
func NewQueueTaggerWithAPI(api TagAPI) *QueueTagger { return &QueueTagger{api: api} }

// Tags implements tagsync.Tagger (ListQueueTags). A queue that does not exist is tagsync.ErrMissing: a
// pool's inbox queue exists only while the pool receives handoffs.
func (q *QueueTagger) Tags(ctx context.Context, url string) (map[string]string, error) {
	out, err := q.api.ListQueueTags(ctx, &awssqs.ListQueueTagsInput{QueueUrl: aws.String(url)})
	if err != nil {
		return nil, tagErr("ListQueueTags", err)
	}
	m := make(map[string]string, len(out.Tags))
	for k, v := range out.Tags {
		m[k] = v
	}
	return m, nil
}

// Tag implements tagsync.Tagger (TagQueue).
func (q *QueueTagger) Tag(ctx context.Context, url string, tags map[string]string) error {
	_, err := q.api.TagQueue(ctx, &awssqs.TagQueueInput{QueueUrl: aws.String(url), Tags: tags})
	return tagErr("TagQueue", err)
}

// Untag implements tagsync.Tagger (UntagQueue).
func (q *QueueTagger) Untag(ctx context.Context, url string, keys []string) error {
	_, err := q.api.UntagQueue(ctx, &awssqs.UntagQueueInput{QueueUrl: aws.String(url), TagKeys: keys})
	return tagErr("UntagQueue", err)
}

func tagErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case queueMissing(err):
		return tagsync.ErrMissing
	}
	return fmt.Errorf("sqs: %s: %w", op, err)
}
