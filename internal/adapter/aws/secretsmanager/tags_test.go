package secretsmanager

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

type fakeTagAPI struct {
	desc    *sm.DescribeSecretOutput
	err     error
	tagIn   *sm.TagResourceInput
	untagIn *sm.UntagResourceInput
}

func (f *fakeTagAPI) DescribeSecret(_ context.Context, _ *sm.DescribeSecretInput, _ ...func(*sm.Options)) (*sm.DescribeSecretOutput, error) {
	return f.desc, f.err
}

func (f *fakeTagAPI) TagResource(_ context.Context, in *sm.TagResourceInput, _ ...func(*sm.Options)) (*sm.TagResourceOutput, error) {
	f.tagIn = in
	return &sm.TagResourceOutput{}, f.err
}

func (f *fakeTagAPI) UntagResource(_ context.Context, in *sm.UntagResourceInput, _ ...func(*sm.Options)) (*sm.UntagResourceOutput, error) {
	f.untagIn = in
	return &sm.UntagResourceOutput{}, f.err
}

const tagARN = "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/ns/_shared/db-AbCdEf"

func TestTaggerTags(t *testing.T) {
	now := time.Now()
	f := &fakeTagAPI{desc: &sm.DescribeSecretOutput{Tags: []types.Tag{{Key: aws.String("a"), Value: aws.String("1")}}}}
	got, err := NewTaggerWithAPI(f).Tags(t.Context(), tagARN)
	if err != nil || got["a"] != "1" || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	f.desc = &sm.DescribeSecretOutput{DeletedDate: &now}
	if _, err := NewTaggerWithAPI(f).Tags(t.Context(), tagARN); !errors.Is(err, tagsync.ErrMissing) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestTaggerWritesAndErrors(t *testing.T) {
	f := &fakeTagAPI{}
	tg := NewTaggerWithAPI(f)
	if err := tg.Tag(t.Context(), tagARN, map[string]string{"env": "dev"}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(f.tagIn.SecretId) != tagARN || len(f.tagIn.Tags) != 1 || aws.ToString(f.tagIn.Tags[0].Key) != "env" {
		t.Errorf("TagResource %+v", f.tagIn)
	}
	if err := tg.Untag(t.Context(), tagARN, []string{"old"}); err != nil || !slices.Equal(f.untagIn.TagKeys, []string{"old"}) {
		t.Errorf("UntagResource %+v %v", f.untagIn, err)
	}
	tests := []struct {
		name    string
		err     error
		missing bool
	}{
		{"not found", &smithy.GenericAPIError{Code: "ResourceNotFoundException"}, true},
		{"marked for deletion", &smithy.GenericAPIError{Code: "InvalidRequestException", Message: "You can't perform this operation on the secret because it was marked for deletion."}, true},
		{"other invalid request", &smithy.GenericAPIError{Code: "InvalidRequestException", Message: "bad"}, false},
		{"throttled", &smithy.GenericAPIError{Code: "ThrottlingException"}, false},
		{"plain", errors.New("network"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTagAPI{err: tc.err}
			err := NewTaggerWithAPI(f).Tag(t.Context(), tagARN, map[string]string{"a": "b"})
			if errors.Is(err, tagsync.ErrMissing) != tc.missing {
				t.Fatalf("err %v", err)
			}
			if !tc.missing && err == nil {
				t.Fatal("want error")
			}
			_, derr := NewTaggerWithAPI(f).Tags(t.Context(), tagARN)
			if errors.Is(derr, tagsync.ErrMissing) != tc.missing {
				t.Fatalf("describe err %v", derr)
			}
		})
	}
}
