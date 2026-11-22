package secretsmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type keysAPI struct {
	fakeWrite
	existing  bool
	scheduled bool
	forced    []string
	putErr    error
}

func (k *keysAPI) CreateSecret(ctx context.Context, in *sm.CreateSecretInput, o ...func(*sm.Options)) (*sm.CreateSecretOutput, error) {
	switch {
	case k.scheduled:
		return nil, &types.InvalidRequestException{Message: aws.String("You can't create this secret because a secret with this name is already scheduled for deletion.")}
	case k.existing:
		return nil, &types.ResourceExistsException{Message: aws.String("dup")}
	}
	return k.fakeWrite.CreateSecret(ctx, in, o...)
}

func (k *keysAPI) PutSecretValue(_ context.Context, in *sm.PutSecretValueInput, _ ...func(*sm.Options)) (*sm.PutSecretValueOutput, error) {
	if k.putErr != nil {
		return nil, k.putErr
	}
	return &sm.PutSecretValueOutput{ARN: aws.String("arn:aws:secretsmanager:us-east-1:111122223333:secret:" + *in.SecretId + "-AbCdEf")}, nil
}

func (k *keysAPI) DeleteSecret(_ context.Context, in *sm.DeleteSecretInput, _ ...func(*sm.Options)) (*sm.DeleteSecretOutput, error) {
	k.forced = append(k.forced, *in.SecretId)
	k.scheduled = false
	return &sm.DeleteSecretOutput{}, nil
}

func TestKeyStorePut(t *testing.T) {
	api := &keysAPI{}
	ks := NewKeyStoreWithAPI(api)
	arn, err := ks.Put(t.Context(), "afs/ns/agt_1/paperclip-agent-api-key", "k1", map[string]string{"afs-team-id": "team_a"})
	if err != nil || !strings.HasSuffix(arn, "paperclip-agent-api-key-AbCdEf") {
		t.Fatalf("%q %v", arn, err)
	}
	if len(api.created) != 1 || len(*api.created[0].ClientRequestToken) != 36 {
		t.Errorf("token: %+v", api.created)
	}
	// Existing: a new version, and the ARN comes from the put.
	api.existing = true
	if arn, err = ks.Put(t.Context(), "afs/ns/agt_1/paperclip-agent-api-key", "k2", nil); err != nil || arn == "" {
		t.Fatalf("existing: %q %v", arn, err)
	}
	api.putErr = errors.New("boom k2")
	if _, err = ks.Put(t.Context(), "n", "k2", nil); err == nil {
		t.Error("put error")
	}
	// Scheduled for deletion: force delete, then create again.
	api.existing, api.scheduled = false, true
	if arn, err = ks.Put(t.Context(), "afs/ns/agt_1/paperclip-agent-api-key", "k3", nil); err != nil || arn == "" || len(api.forced) != 1 {
		t.Fatalf("scheduled: %q %v %v", arn, err, api.forced)
	}
	// Other create errors surface without the value.
	api.err = &types.InternalServiceError{Message: aws.String("x k4 x")}
	if _, err = ks.Put(t.Context(), "n", "k4", nil); err == nil {
		t.Error("create error")
	}
	if err = ks.ScheduleDeletion(t.Context(), "arn:aws:secretsmanager:us-east-1:111122223333:secret:n-AbCdEf"); err != nil {
		t.Error(err)
	}
}

type descAPI struct {
	out *sm.DescribeSecretOutput
	err error
}

func (d descAPI) DescribeSecret(context.Context, *sm.DescribeSecretInput, ...func(*sm.Options)) (*sm.DescribeSecretOutput, error) {
	return d.out, d.err
}

func TestFinder(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		api   descAPI
		found bool
		err   bool
	}{
		{"found", descAPI{out: &sm.DescribeSecretOutput{ARN: aws.String("arn:x"), VersionIdsToStages: map[string][]string{"v": {"AWSCURRENT"}}}}, true, false},
		{"no value yet", descAPI{out: &sm.DescribeSecretOutput{ARN: aws.String("arn:x")}}, false, false},
		{"deleted", descAPI{out: &sm.DescribeSecretOutput{ARN: aws.String("arn:x"), DeletedDate: &now, VersionIdsToStages: map[string][]string{"v": {"AWSCURRENT"}}}}, false, false},
		{"missing", descAPI{err: &types.ResourceNotFoundException{}}, false, false},
		{"error", descAPI{err: errors.New("denied")}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arn, ok, err := NewFinderWithAPI(tc.api).ARN(t.Context(), "n")
			if ok != tc.found || (err != nil) != tc.err || ok && arn != "arn:x" {
				t.Errorf("%q %v %v", arn, ok, err)
			}
		})
	}
}
