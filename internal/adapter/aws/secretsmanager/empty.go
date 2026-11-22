package secretsmanager

import (
	"context"
	"errors"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// EmptyAPI is the subset of the SDK client EmptyCreator calls.
type EmptyAPI interface {
	CreateSecret(ctx context.Context, in *sm.CreateSecretInput, opts ...func(*sm.Options)) (*sm.CreateSecretOutput, error)
	DescribeSecret(ctx context.Context, in *sm.DescribeSecretInput, opts ...func(*sm.Options)) (*sm.DescribeSecretOutput, error)
}

// EmptyCreator creates secrets that have no value yet: the secrets a swarm template declares for a remediation
// team (M6-E, RC-12). The team puts the value afterwards with PutSecretValue; this type never takes, reads or
// copies one, so it implements workorder.SecretCreator without a way to leak a value.
type EmptyCreator struct{ api EmptyAPI }

// NewEmptyCreator returns an EmptyCreator on a configured SDK config.
func NewEmptyCreator(cfg aws.Config) *EmptyCreator { return &EmptyCreator{api: sm.NewFromConfig(cfg)} }

// NewEmptyCreatorWithAPI returns an EmptyCreator on api (tests).
func NewEmptyCreatorWithAPI(api EmptyAPI) *EmptyCreator { return &EmptyCreator{api: api} }

// CreateEmpty creates the secret name with the tags and no value, and returns its ARN. A secret of that name that
// already exists (a retry after the database record failed) is not an error: its ARN is returned.
func (c *EmptyCreator) CreateEmpty(ctx context.Context, name string, tags map[string]string) (string, error) {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tg := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		tg = append(tg, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	out, err := c.api.CreateSecret(ctx, &sm.CreateSecretInput{Name: aws.String(name), Tags: tg})
	if err == nil {
		return aws.ToString(out.ARN), nil
	}
	var ex *types.ResourceExistsException
	if !errors.As(err, &ex) {
		return "", wrap(err)
	}
	d, derr := c.api.DescribeSecret(ctx, &sm.DescribeSecretInput{SecretId: aws.String(name)})
	if derr != nil {
		return "", wrap(derr)
	}
	return aws.ToString(d.ARN), nil
}
