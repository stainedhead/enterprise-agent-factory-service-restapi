package secretsmanager_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
)

type emptyFake struct {
	create    *sm.CreateSecretInput
	createErr error
	described string
	descErr   error
}

func (f *emptyFake) CreateSecret(_ context.Context, in *sm.CreateSecretInput, _ ...func(*sm.Options)) (*sm.CreateSecretOutput, error) {
	f.create = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &sm.CreateSecretOutput{ARN: aws.String("arn:aws:secretsmanager:us-east-1:111122223333:secret:" + aws.ToString(in.Name) + "-AbCdEf")}, nil
}

func (f *emptyFake) DescribeSecret(_ context.Context, in *sm.DescribeSecretInput, _ ...func(*sm.Options)) (*sm.DescribeSecretOutput, error) {
	f.described = aws.ToString(in.SecretId)
	if f.descErr != nil {
		return nil, f.descErr
	}
	return &sm.DescribeSecretOutput{ARN: aws.String("arn:aws:secretsmanager:us-east-1:111122223333:secret:" + f.described + "-ZzZzZz")}, nil
}

// M6-E (RC-12 gap): a template secret is created with no value and the team tags.
func TestCreateEmpty(t *testing.T) {
	f := &emptyFake{}
	arn, err := secretsmanager.NewEmptyCreatorWithAPI(f).CreateEmpty(t.Context(), "afs/afs-t-wo-x/triage/scanner-token", map[string]string{"afs-team-id": "team_1", "afs-pool-id": "pool_1"})
	if err != nil || arn != "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-wo-x/triage/scanner-token-AbCdEf" {
		t.Fatalf("arn = %q, %v", arn, err)
	}
	if f.create.SecretString != nil || f.create.SecretBinary != nil {
		t.Error("a template secret is created with no value")
	}
	if len(f.create.Tags) != 2 || aws.ToString(f.create.Tags[0].Key) != "afs-pool-id" || aws.ToString(f.create.Tags[1].Key) != "afs-team-id" {
		t.Errorf("tags = %+v: sorted, the team tag among them", f.create.Tags)
	}
}

func TestCreateEmptyIsIdempotentAndWrapsErrors(t *testing.T) {
	f := &emptyFake{createErr: &types.ResourceExistsException{}}
	arn, err := secretsmanager.NewEmptyCreatorWithAPI(f).CreateEmpty(t.Context(), "afs/x/y/z", nil)
	if err != nil || arn != "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/y/z-ZzZzZz" || f.described != "afs/x/y/z" {
		t.Errorf("existing: %q, %v (described %q)", arn, err, f.described)
	}
	f = &emptyFake{createErr: &types.ResourceExistsException{}, descErr: errors.New("denied")}
	if _, err := secretsmanager.NewEmptyCreatorWithAPI(f).CreateEmpty(t.Context(), "n", nil); err == nil {
		t.Error("a describe failure must be returned")
	}
	f = &emptyFake{createErr: errors.New("throttled")}
	if _, err := secretsmanager.NewEmptyCreatorWithAPI(f).CreateEmpty(t.Context(), "n", nil); err == nil || f.described != "" {
		t.Errorf("a create failure is returned without a lookup: %v", err)
	}
}
