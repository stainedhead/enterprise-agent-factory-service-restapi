package eventbridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

type fakeAPI struct {
	in  *eb.PutEventsInput
	out *eb.PutEventsOutput
	err error
}

func (f *fakeAPI) PutEvents(_ context.Context, in *eb.PutEventsInput, _ ...func(*eb.Options)) (*eb.PutEventsOutput, error) {
	f.in = in
	return f.out, f.err
}

func entries(n int) []housekeeping.Entry {
	var out []housekeeping.Entry
	for i := 0; i < n; i++ {
		out = append(out, housekeeping.Entry{Key: int64(i), DetailType: "team.created", Detail: json.RawMessage(`{"id":"evt_1"}`),
			Time: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), Resources: []string{"team/team_1"}})
	}
	return out
}

func TestPutEventsShape(t *testing.T) {
	f := &fakeAPI{out: &eb.PutEventsOutput{Entries: []types.PutEventsResultEntry{{EventId: aws.String("1")}}}}
	res, err := NewWithAPI(f, "afs-bus").PutEvents(t.Context(), entries(1))
	if err != nil || len(res) != 1 || res[0] != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	e := f.in.Entries[0]
	if aws.ToString(e.Source) != "afs" || aws.ToString(e.DetailType) != "team.created" || aws.ToString(e.EventBusName) != "afs-bus" ||
		aws.ToString(e.Detail) != `{"id":"evt_1"}` || e.Resources[0] != "team/team_1" || e.Time == nil {
		t.Fatalf("entry %+v", e)
	}
}

func TestPutEventsPartialFailure(t *testing.T) {
	f := &fakeAPI{out: &eb.PutEventsOutput{FailedEntryCount: 1, Entries: []types.PutEventsResultEntry{
		{EventId: aws.String("1")},
		{ErrorCode: aws.String("ThrottlingException"), ErrorMessage: aws.String("slow down")},
	}}}
	res, err := NewWithAPI(f, "b").PutEvents(t.Context(), entries(2))
	if err != nil || res[0] != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	var ee *EntryError
	if !errors.As(res[1], &ee) || ee.Code != "ThrottlingException" {
		t.Fatalf("res[1] = %v", res[1])
	}
}

func TestPutEventsErrors(t *testing.T) {
	tests := []struct {
		name string
		c    *Client
		n    int
	}{
		{"call fails", NewWithAPI(&fakeAPI{err: errors.New("reset")}, "b"), 1},
		{"no bus", NewWithAPI(&fakeAPI{}, ""), 1},
		{"over the limit", NewWithAPI(&fakeAPI{}, "b"), 11},
		{"short response", NewWithAPI(&fakeAPI{out: &eb.PutEventsOutput{}}, "b"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.c.PutEvents(t.Context(), entries(tt.n)); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if res, err := NewWithAPI(&fakeAPI{}, "b").PutEvents(t.Context(), nil); err != nil || res != nil {
		t.Fatalf("empty: %v %v", res, err)
	}
}
