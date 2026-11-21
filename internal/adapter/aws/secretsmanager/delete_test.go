package secretsmanager

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type fakeAPI struct {
	deleted  []*sm.DeleteSecretInput
	delErr   error
	pages    []*sm.ListSecretsOutput
	listIns  []*sm.ListSecretsInput
	listErr  error
	pageNext int
}

func (f *fakeAPI) DeleteSecret(_ context.Context, in *sm.DeleteSecretInput, _ ...func(*sm.Options)) (*sm.DeleteSecretOutput, error) {
	f.deleted = append(f.deleted, in)
	return &sm.DeleteSecretOutput{}, f.delErr
}

func (f *fakeAPI) ListSecrets(_ context.Context, in *sm.ListSecretsInput, _ ...func(*sm.Options)) (*sm.ListSecretsOutput, error) {
	cp := *in
	f.listIns = append(f.listIns, &cp)
	if f.listErr != nil {
		return nil, f.listErr
	}
	p := f.pages[f.pageNext]
	f.pageNext++
	return p, nil
}

func TestScheduleDeletion(t *testing.T) {
	arn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:t/x-AbCdEf"
	tests := []struct {
		name    string
		days    int
		err     error
		wantErr bool
		wantAPI bool
		code    string
	}{
		{"ok", 30, nil, false, true, ""},
		{"window too short", 3, nil, true, false, ""},
		{"window too long", 31, nil, true, false, ""},
		{"already scheduled", 30, &types.InvalidRequestException{Message: aws.String("You can't perform this operation on the secret because it was marked for deletion. It is currently scheduled for deletion")}, false, true, ""},
		{"not found", 30, &types.ResourceNotFoundException{Message: aws.String("gone")}, false, true, ""},
		{"other invalid request", 30, &types.InvalidRequestException{Message: aws.String("replica secret")}, true, true, "InvalidRequestException"},
		{"throttled", 30, &types.InternalServiceError{Message: aws.String("slow")}, true, true, "InternalServiceError"},
		{"transport", 30, errors.New("connection reset"), true, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeAPI{delErr: tt.err}
			err := NewWithAPI(f).ScheduleDeletion(context.Background(), arn, tt.days)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if (len(f.deleted) == 1) != tt.wantAPI {
				t.Fatalf("api calls = %d", len(f.deleted))
			}
			if tt.wantAPI {
				in := f.deleted[0]
				if aws.ToString(in.SecretId) != arn || aws.ToInt64(in.RecoveryWindowInDays) != int64(tt.days) {
					t.Errorf("input = %+v", in)
				}
				if in.ForceDeleteWithoutRecovery != nil && *in.ForceDeleteWithoutRecovery {
					t.Error("force delete must never be set")
				}
			}
			var ce interface{ Code() string }
			if tt.code != "" && (!errors.As(err, &ce) || ce.Code() != tt.code) {
				t.Errorf("error %v has no code %s", err, tt.code)
			}
		})
	}
}

func TestListTeamSecretsPaginatesAndConfirmsTag(t *testing.T) {
	tag := func(k, v string) types.Tag { return types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	f := &fakeAPI{pages: []*sm.ListSecretsOutput{
		{SecretList: []types.SecretListEntry{
			{ARN: aws.String("a"), Tags: []types.Tag{tag("afs-team-id", "team_01")}},
			// matches the tag key and (another tag's) value but not as a pair
			{ARN: aws.String("decoy"), Tags: []types.Tag{tag("afs-team-id", "team_02"), tag("owner", "team_01")}},
		}, NextToken: aws.String("p2")},
		{SecretList: []types.SecretListEntry{{ARN: aws.String("b"), Tags: []types.Tag{tag("afs-team-id", "team_01")}}}},
	}}
	got, err := NewWithAPI(f).ListTeamSecrets(context.Background(), "team_01")
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %v err %v", got, err)
	}
	if len(f.listIns) != 2 || aws.ToString(f.listIns[1].NextToken) != "p2" {
		t.Errorf("pagination not followed: %+v", f.listIns)
	}
	if len(f.listIns[0].Filters) != 2 {
		t.Errorf("filters = %+v", f.listIns[0].Filters)
	}
	f2 := &fakeAPI{listErr: &types.InternalServiceError{Message: aws.String("x")}}
	if _, err := NewWithAPI(f2).ListTeamSecrets(context.Background(), "t"); err == nil {
		t.Fatal("want error")
	}
}
