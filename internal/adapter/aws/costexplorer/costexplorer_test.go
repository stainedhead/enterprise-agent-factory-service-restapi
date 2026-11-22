package costexplorer

import (
	"context"
	"errors"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

type fakeAPI struct {
	pages []*ce.ListCostAllocationTagsOutput
	err   error
	calls []*ce.ListCostAllocationTagsInput
}

func (f *fakeAPI) ListCostAllocationTags(_ context.Context, in *ce.ListCostAllocationTagsInput, _ ...func(*ce.Options)) (*ce.ListCostAllocationTagsOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	p := f.pages[len(f.calls)-1]
	return p, nil
}

func tag(k string, st types.CostAllocationTagStatus, upd string) types.CostAllocationTag {
	t := types.CostAllocationTag{TagKey: awssdk.String(k), Status: st, Type: types.CostAllocationTagTypeUserDefined}
	if upd != "" {
		t.LastUpdatedDate = awssdk.String(upd)
	}
	return t
}

func TestListTags(t *testing.T) {
	api := &fakeAPI{pages: []*ce.ListCostAllocationTagsOutput{
		{CostAllocationTags: []types.CostAllocationTag{tag("afs-team-id", types.CostAllocationTagStatusActive, "2026-11-01T00:00:00Z")}, NextToken: awssdk.String("n1")},
		{CostAllocationTags: []types.CostAllocationTag{
			tag("afs-agent-id", types.CostAllocationTagStatusInactive, "2026-11-02"),
			tag("cost-centre", types.CostAllocationTagStatusActive, "garbage"),
			{Status: types.CostAllocationTagStatusActive}, // no key: skipped
		}},
	}}
	got, err := NewWithAPI(api).ListTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 2 || api.calls[1].NextToken == nil || *api.calls[1].NextToken != "n1" {
		t.Fatalf("pagination: %+v", api.calls)
	}
	if api.calls[0].Type != types.CostAllocationTagTypeUserDefined {
		t.Errorf("type = %q", api.calls[0].Type)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if a := got["afs-team-id"]; !a.Active || !a.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("team: %+v", a)
	}
	if a := got["afs-agent-id"]; a.Active || !a.Since.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("agent: %+v", a)
	}
	if a := got["cost-centre"]; !a.Active || !a.Since.IsZero() {
		t.Errorf("unparsable date should give zero Since: %+v", a)
	}
}

func TestListTagsErrors(t *testing.T) {
	boom := errors.New("denied")
	if _, err := NewWithAPI(&fakeAPI{err: boom}).ListTags(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	loop := &fakeAPI{}
	for range maxPages + 1 {
		loop.pages = append(loop.pages, &ce.ListCostAllocationTagsOutput{NextToken: awssdk.String("x")})
	}
	if _, err := NewWithAPI(loop).ListTags(context.Background()); err == nil {
		t.Error("an endless pagination must stop")
	}
}
