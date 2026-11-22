package secretsmanager

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

// TagAPI is the subset of the Secrets Manager SDK client the tag sync calls.
type TagAPI interface {
	DescribeSecret(ctx context.Context, in *sm.DescribeSecretInput, opts ...func(*sm.Options)) (*sm.DescribeSecretOutput, error)
	TagResource(ctx context.Context, in *sm.TagResourceInput, opts ...func(*sm.Options)) (*sm.TagResourceOutput, error)
	UntagResource(ctx context.Context, in *sm.UntagResourceInput, opts ...func(*sm.Options)) (*sm.UntagResourceOutput, error)
}

// Tagger implements tagsync.Tagger for secrets addressed by ARN. It reads and writes tags only, never a
// secret value.
type Tagger struct{ api TagAPI }

var _ tagsync.Tagger = (*Tagger)(nil)

// NewTagger returns a Tagger on a configured SDK config.
func NewTagger(cfg aws.Config) *Tagger { return &Tagger{api: sm.NewFromConfig(cfg)} }

// NewTaggerWithAPI returns a Tagger on api (tests).
func NewTaggerWithAPI(api TagAPI) *Tagger { return &Tagger{api: api} }

// Tags implements tagsync.Tagger (DescribeSecret). A secret that does not exist, or is scheduled for
// deletion, is tagsync.ErrMissing.
func (t *Tagger) Tags(ctx context.Context, arn string) (map[string]string, error) {
	out, err := t.api.DescribeSecret(ctx, &sm.DescribeSecretInput{SecretId: aws.String(arn)})
	if err != nil {
		return nil, tagError(err)
	}
	if out.DeletedDate != nil {
		return nil, tagsync.ErrMissing
	}
	m := make(map[string]string, len(out.Tags))
	for _, tg := range out.Tags {
		m[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	return m, nil
}

// Tag implements tagsync.Tagger (TagResource).
func (t *Tagger) Tag(ctx context.Context, arn string, tags map[string]string) error {
	in := &sm.TagResourceInput{SecretId: aws.String(arn)}
	for k, v := range tags {
		in.Tags = append(in.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := t.api.TagResource(ctx, in)
	return tagError(err)
}

// Untag implements tagsync.Tagger (UntagResource).
func (t *Tagger) Untag(ctx context.Context, arn string, keys []string) error {
	_, err := t.api.UntagResource(ctx, &sm.UntagResourceInput{SecretId: aws.String(arn), TagKeys: keys})
	return tagError(err)
}

// tagError maps a missing secret (or one scheduled for deletion, which refuses tag changes) to
// tagsync.ErrMissing and wraps the rest.
func tagError(err error) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "ResourceNotFoundException":
			return tagsync.ErrMissing
		case "InvalidRequestException":
			if strings.Contains(ae.ErrorMessage(), "marked for deletion") || strings.Contains(ae.ErrorMessage(), "scheduled for deletion") {
				return tagsync.ErrMissing
			}
		}
	}
	return wrap(err)
}
