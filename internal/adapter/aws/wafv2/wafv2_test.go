package wafv2

import (
	"context"
	"errors"
	"testing"

	awswaf "github.com/aws/aws-sdk-go-v2/service/wafv2"
	"github.com/aws/aws-sdk-go-v2/service/wafv2/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

type fakeAPI struct {
	addrs   []string
	updated *awswaf.UpdateIPSetInput
	getErr  error
	putErr  error
	gets    []*awswaf.GetIPSetInput
}

func (f *fakeAPI) GetIPSet(_ context.Context, in *awswaf.GetIPSetInput, _ ...func(*awswaf.Options)) (*awswaf.GetIPSetOutput, error) {
	f.gets = append(f.gets, in)
	if f.getErr != nil {
		return nil, f.getErr
	}
	tok := "lock-1"
	return &awswaf.GetIPSetOutput{IPSet: &types.IPSet{Addresses: f.addrs}, LockToken: &tok}, nil
}

func (f *fakeAPI) UpdateIPSet(_ context.Context, in *awswaf.UpdateIPSetInput, _ ...func(*awswaf.Options)) (*awswaf.UpdateIPSetOutput, error) {
	f.updated = in
	return &awswaf.UpdateIPSetOutput{}, f.putErr
}

const arn4 = "arn:aws:wafv2:us-east-1:111122223333:regional/ipset/afs-collab-v4/a1b2c3d4"

func TestSets(t *testing.T) {
	f := &fakeAPI{addrs: []string{"10.0.0.0/8"}}
	s := NewWithAPI(f)
	got, err := s.Addresses(t.Context(), collab.IPSetRef{ARN: arn4})
	if err != nil || len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("%v %v", got, err)
	}
	if *f.gets[0].Name != "afs-collab-v4" || *f.gets[0].Id != "a1b2c3d4" || f.gets[0].Scope != types.ScopeRegional {
		t.Errorf("get input: %+v", f.gets[0])
	}
	if err := s.Update(t.Context(), collab.IPSetRef{ARN: arn4}, []string{"192.0.2.0/24"}); err != nil {
		t.Fatal(err)
	}
	if *f.updated.LockToken != "lock-1" || f.updated.Addresses[0] != "192.0.2.0/24" {
		t.Errorf("update input: %+v", f.updated)
	}
	f.putErr = errors.New("stale lock")
	if err := s.Update(t.Context(), collab.IPSetRef{ARN: arn4}, nil); err == nil {
		t.Error("update failure swallowed")
	}
	f.getErr = errors.New("denied")
	if _, err := s.Addresses(t.Context(), collab.IPSetRef{ARN: arn4}); err == nil {
		t.Error("read failure swallowed")
	}
	if err := s.Update(t.Context(), collab.IPSetRef{ARN: arn4}, nil); err == nil {
		t.Error("read-before-write failure swallowed")
	}
}

func TestParts(t *testing.T) {
	if sc, _, _, err := parts("arn:aws:wafv2:us-east-1:111122223333:global/ipset/n/i"); err != nil || sc != types.ScopeCloudfront {
		t.Errorf("global: %v %v", sc, err)
	}
	for _, bad := range []string{"", "arn:aws:s3:::x", "arn:aws:wafv2:us-east-1:1:regional/webacl/n/i", "arn:aws:wafv2:us-east-1:1:other/ipset/n/i", "arn:aws:wafv2:us-east-1:1:regional/ipset//i"} {
		if _, _, _, err := parts(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	s := NewWithAPI(&fakeAPI{})
	if _, err := s.Addresses(t.Context(), collab.IPSetRef{ARN: "x"}); err == nil {
		t.Error("bad ARN accepted by Addresses")
	}
	if err := s.Update(t.Context(), collab.IPSetRef{ARN: "x"}, nil); err == nil {
		t.Error("bad ARN accepted by Update")
	}
}
