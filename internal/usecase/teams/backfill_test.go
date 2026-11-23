package teams_test

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// RA-US-21: the backfill creates the agent of each team without one, once; a rerun changes nothing.
func TestBackfill(t *testing.T) {
	r := newRig(t)
	var ids []string
	for i := 0; i < 3; i++ {
		in := createInput()
		in.Params.Slug = []string{"alpha", "bravo", "charlie"}[i]
		res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Team.ID)
	}
	// Two teams predate 1.1.0 (no agent); the third is deleting and is left alone.
	for _, id := range ids {
		tm := r.repo.Teams[id]
		tm.OrchestrationAgentID = ""
		r.repo.Teams[id] = tm
	}
	del := r.repo.Teams[ids[2]]
	del.State = domain.TeamStateDeleting
	r.repo.Teams[ids[2]] = del
	r.repo.Orchestrations = nil

	res, err := r.svc.Backfill(t.Context(), nil)
	if err != nil || res.Created != 2 || len(res.Failed) != 0 {
		t.Fatalf("backfill: %+v %v", res, err)
	}
	if len(r.repo.Orchestrations) != 2 || r.repo.Teams[ids[0]].OrchestrationAgentID == "" || r.repo.Teams[ids[2]].OrchestrationAgentID != "" {
		t.Fatalf("agents: %+v", r.repo.Orchestrations)
	}
	again, err := r.svc.Backfill(t.Context(), nil)
	if err != nil || again.Created != 0 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
}

// A team whose agent cannot be created is reported and the run goes on; a rerun retries it.
func TestBackfillFailureContinues(t *testing.T) {
	r := newRig(t)
	res, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := r.repo.Teams[res.Team.ID]
	tm.OrchestrationAgentID = ""
	r.repo.Teams[res.Team.ID] = tm
	r.repo.OrchErr = errors.New("boom")
	out, err := r.svc.Backfill(t.Context(), nil)
	if err == nil || len(out.Failed) != 1 || out.Remaining != 1 || out.Created != 0 {
		t.Fatalf("failing run: %+v %v", out, err)
	}
	r.repo.OrchErr = nil
	out, err = r.svc.Backfill(t.Context(), nil)
	if err != nil || out.Created != 1 {
		t.Fatalf("retry: %+v %v", out, err)
	}
}
