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

type fakeWrite struct {
	fakeAPI
	created []*sm.CreateSecretInput
	puts    []*sm.PutSecretValueInput
	tags    []*sm.TagResourceInput
	err     error
}

func (f *fakeWrite) CreateSecret(_ context.Context, in *sm.CreateSecretInput, _ ...func(*sm.Options)) (*sm.CreateSecretOutput, error) {
	f.created = append(f.created, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sm.CreateSecretOutput{ARN: aws.String("arn:aws:secretsmanager:us-east-1:111122223333:secret:" + *in.Name + "-AbCdEf"), VersionId: aws.String("v1")}, nil
}

func (f *fakeWrite) PutSecretValue(_ context.Context, in *sm.PutSecretValueInput, _ ...func(*sm.Options)) (*sm.PutSecretValueOutput, error) {
	f.puts = append(f.puts, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sm.PutSecretValueOutput{VersionId: aws.String("v2")}, nil
}

func (f *fakeWrite) TagResource(_ context.Context, in *sm.TagResourceInput, _ ...func(*sm.Options)) (*sm.TagResourceOutput, error) {
	f.tags = append(f.tags, in)
	return &sm.TagResourceOutput{}, f.err
}

func TestWriterCreate(t *testing.T) {
	f := &fakeWrite{}
	w := NewWriterWithAPI(f)
	got, err := w.Create(t.Context(), "afs/ns/_shared/pat", "the-value", "tok", map[string]string{"afs-team-id": "team_1", "afs-agent-id": "agt_1"})
	if err != nil || got.VersionID != "v1" || !strings.HasSuffix(got.ARN, "pat-AbCdEf") {
		t.Fatalf("%+v %v", got, err)
	}
	in := f.created[0]
	if *in.Name != "afs/ns/_shared/pat" || *in.ClientRequestToken != "tok" || len(in.Tags) != 2 || *in.Tags[0].Key != "afs-agent-id" {
		t.Errorf("input: %+v", in)
	}
}

func TestWriterErrors(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		taken bool
		code  string
	}{
		{"exists", &types.ResourceExistsException{Message: aws.String("dup")}, true, ""},
		{"throttle", &types.InternalServiceError{Message: aws.String("x the-value x")}, false, "InternalServiceError"},
		{"transport", errors.New("reset"), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriterWithAPI(&fakeWrite{err: tt.err})
			_, err := w.Create(t.Context(), "n", "the-value", "tok", nil)
			var nt *NameTakenError
			if errors.As(err, &nt) != tt.taken || err == nil {
				t.Fatalf("err = %v", err)
			}
			if _, err := w.PutValue(t.Context(), "arn", "the-value", "tok"); err == nil {
				t.Error("PutValue must fail")
			}
		})
	}
}

func TestWriterPutForceDeleteTag(t *testing.T) {
	f := &fakeWrite{}
	w := NewWriterWithAPI(f)
	v, err := w.PutValue(t.Context(), "arn", "val", "tok")
	if err != nil || v != "v2" || *f.puts[0].SecretId != "arn" {
		t.Fatal(v, err)
	}
	if err := w.ForceDelete(t.Context(), "arn"); err != nil || !*f.deleted[0].ForceDeleteWithoutRecovery {
		t.Fatal(err)
	}
	f.delErr = &types.ResourceNotFoundException{}
	if err := w.ForceDelete(t.Context(), "arn"); err != nil {
		t.Error("missing is success")
	}
	f.delErr = errors.New("boom")
	if err := w.ForceDelete(t.Context(), "arn"); err == nil {
		t.Error("error expected")
	}
	if err := w.TagOrphan(t.Context(), "arn"); err != nil || *f.tags[0].Tags[0].Key != OrphanTagKey {
		t.Fatal(err)
	}
}

func TestRequestToken(t *testing.T) {
	a, b := RequestToken("p", "k"), RequestToken("p", "k")
	if a != b || len(a) != 36 || a == RequestToken("pk", "") || a == RequestToken("p", "k2") {
		t.Errorf("token %s", a)
	}
}

func TestListUnderPrefix(t *testing.T) {
	now := time.Now()
	f := &fakeWrite{fakeAPI: fakeAPI{pages: []*sm.ListSecretsOutput{
		{SecretList: []types.SecretListEntry{
			{ARN: aws.String("a1"), Name: aws.String("afs/ns/_shared/x"), CreatedDate: &now},
			{ARN: aws.String("a2"), Name: aws.String("afs/ns/_shared/y"), Tags: []types.Tag{{Key: aws.String("afs-orphan"), Value: aws.String("true")}}},
			{ARN: aws.String("a3"), Name: aws.String("other/z")},
			{ARN: aws.String("a4"), Name: aws.String("afs/ns/_shared/gone"), DeletedDate: &now},
		}, NextToken: aws.String("n")},
		{SecretList: []types.SecretListEntry{{ARN: aws.String("a5"), Name: aws.String("afs/ns/b/w")}}},
	}}}
	got, err := NewWriterWithAPI(f).ListUnderPrefix(t.Context(), "afs")
	if err != nil || len(got) != 3 || !got[1].Orphan || got[0].Created.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	f2 := &fakeWrite{fakeAPI: fakeAPI{listErr: errors.New("x")}}
	if _, err := NewWriterWithAPI(f2).ListUnderPrefix(t.Context(), "afs"); err == nil {
		t.Error("error expected")
	}
}
