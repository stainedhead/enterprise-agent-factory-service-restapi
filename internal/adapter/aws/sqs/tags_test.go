package sqs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

type fakeTagAPI struct {
	tags    map[string]string
	err     error
	tagIn   *awssqs.TagQueueInput
	untagIn *awssqs.UntagQueueInput
}

func (f *fakeTagAPI) ListQueueTags(context.Context, *awssqs.ListQueueTagsInput, ...func(*awssqs.Options)) (*awssqs.ListQueueTagsOutput, error) {
	return &awssqs.ListQueueTagsOutput{Tags: f.tags}, f.err
}

func (f *fakeTagAPI) TagQueue(_ context.Context, in *awssqs.TagQueueInput, _ ...func(*awssqs.Options)) (*awssqs.TagQueueOutput, error) {
	f.tagIn = in
	return &awssqs.TagQueueOutput{}, f.err
}

func (f *fakeTagAPI) UntagQueue(_ context.Context, in *awssqs.UntagQueueInput, _ ...func(*awssqs.Options)) (*awssqs.UntagQueueOutput, error) {
	f.untagIn = in
	return &awssqs.UntagQueueOutput{}, f.err
}

const tagURL = "https://sqs.us-east-1.amazonaws.com/111122223333/afs-q"

func TestQueueTagger(t *testing.T) {
	f := &fakeTagAPI{tags: map[string]string{"a": "1"}}
	q := NewQueueTaggerWithAPI(f)
	if got, err := q.Tags(t.Context(), tagURL); err != nil || got["a"] != "1" {
		t.Fatalf("%v %v", got, err)
	}
	if err := q.Tag(t.Context(), tagURL, map[string]string{"env": "dev"}); err != nil || aws.ToString(f.tagIn.QueueUrl) != tagURL || f.tagIn.Tags["env"] != "dev" {
		t.Fatalf("TagQueue %+v %v", f.tagIn, err)
	}
	if err := q.Untag(t.Context(), tagURL, []string{"old"}); err != nil || !slices.Equal(f.untagIn.TagKeys, []string{"old"}) {
		t.Fatalf("UntagQueue %+v %v", f.untagIn, err)
	}
}

func TestQueueTaggerErrors(t *testing.T) {
	missing := &smithy.GenericAPIError{Code: "AWS.SimpleQueueService.NonExistentQueue"}
	f := &fakeTagAPI{err: missing}
	q := NewQueueTaggerWithAPI(f)
	if _, err := q.Tags(t.Context(), tagURL); !errors.Is(err, tagsync.ErrMissing) {
		t.Errorf("Tags: %v", err)
	}
	if err := q.Tag(t.Context(), tagURL, nil); !errors.Is(err, tagsync.ErrMissing) {
		t.Errorf("Tag: %v", err)
	}
	if err := q.Untag(t.Context(), tagURL, nil); !errors.Is(err, tagsync.ErrMissing) {
		t.Errorf("Untag: %v", err)
	}
	f.err = errors.New("network")
	for name, err := range map[string]error{"Tags": func() error { _, e := q.Tags(t.Context(), tagURL); return e }(), "Tag": q.Tag(t.Context(), tagURL, nil), "Untag": q.Untag(t.Context(), tagURL, nil)} {
		if err == nil || errors.Is(err, tagsync.ErrMissing) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
