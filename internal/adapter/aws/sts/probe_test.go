package sts

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
)

type recAPI struct {
	in  []*awssts.AssumeRoleInput
	err error
}

func (f *recAPI) AssumeRole(_ context.Context, in *awssts.AssumeRoleInput, _ ...func(*awssts.Options)) (*awssts.AssumeRoleOutput, error) {
	f.in = append(f.in, in)
	if f.err != nil {
		return nil, f.err
	}
	return &awssts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: awssdk.String("AKIAEXAMPLE"), SecretAccessKey: awssdk.String("secret"),
		SessionToken: awssdk.String("tok"), Expiration: awssdk.Time(time.Now().Add(time.Hour))}}, nil
}

var req = links.VerifyRequest{TeamID: "team_1", AgentID: "agt_1", AgentRoleARN: "arn:aws:iam::999988887777:role/afs/ns/afs-1",
	TargetRoleARN: "arn:aws:iam::111122223333:role/d", ExternalID: "afs-ext", SessionName: "agt_1", DurationSeconds: 3600}

func TestProbeAssumesTheAgentRoleThenTheLinkWithExternalIDAndTags(t *testing.T) {
	first, second := &recAPI{}, &recAPI{}
	var gotCreds awssdk.Credentials
	p := NewProbeWithAPI(first, func(c awssdk.Credentials) API { gotCreds = c; return second })
	if err := p.Verify(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	a, b := first.in[0], second.in[0]
	if awssdk.ToString(a.RoleArn) != req.AgentRoleARN || awssdk.ToString(a.RoleSessionName) != ProbeSessionName {
		t.Fatalf("hop 1: %+v", a)
	}
	if gotCreds.AccessKeyID != "AKIAEXAMPLE" || gotCreds.SessionToken != "tok" {
		t.Fatalf("hop 2 must use hop 1 credentials: %+v", gotCreds)
	}
	if awssdk.ToString(b.RoleArn) != req.TargetRoleARN || awssdk.ToString(b.ExternalId) != "afs-ext" || awssdk.ToString(b.RoleSessionName) != "agt_1" {
		t.Fatalf("hop 2: %+v", b)
	}
	tags := map[string]string{}
	for _, tg := range b.Tags {
		tags[awssdk.ToString(tg.Key)] = awssdk.ToString(tg.Value)
	}
	if tags["afs-team-id"] != "team_1" || tags["afs-agent-id"] != "agt_1" || len(tags) != 2 {
		t.Fatalf("session tags: %v", tags)
	}
}

func TestProbeKeepsTheAWSErrorCode(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized"}
	for name, tc := range map[string]struct{ first, second error }{"agent role": {denied, nil}, "linked role": {nil, denied}} {
		p := NewProbeWithAPI(&recAPI{err: tc.first}, func(awssdk.Credentials) API { return &recAPI{err: tc.second} })
		err := p.Verify(t.Context(), req)
		var coded interface{ ErrorCode() string }
		if err == nil || !errors.As(err, &coded) || coded.ErrorCode() != "AccessDenied" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := NewProbeWithAPI(&recAPI{}, nil).Verify(t.Context(), links.VerifyRequest{}); err == nil {
		t.Fatal("incomplete request")
	}
}
