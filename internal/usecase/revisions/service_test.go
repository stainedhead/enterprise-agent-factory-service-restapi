package revisions_test

import (
	"context"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/revisions"
)

type repo struct{ rows []revisions.Revision }

func (r *repo) List(_ context.Context, _ port.Tx, _ string, f revisions.Filter, before int64, limit int) ([]revisions.Revision, error) {
	var out []revisions.Revision
	for _, v := range r.rows {
		if (before == 0 || v.Revision < before) && (f.ResourceKind == "" || v.ResourceKind == f.ResourceKind) {
			out = append(out, v)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, _ string, rev int64) (revisions.Revision, error) {
	for _, v := range r.rows {
		if v.Revision == rev {
			v.Snapshot = []byte(`{}`)
			return v, nil
		}
	}
	return revisions.Revision{}, domain.NotFound("revision")
}

func (r *repo) TeamExists(_ context.Context, _ port.Tx, id string) (bool, error) {
	return id == "team_1", nil
}

func rig() *revisions.Service {
	r := &repo{}
	for i := int64(3); i >= 1; i-- {
		kind := "teams"
		if i == 2 {
			kind = "team_tags"
		}
		r.rows = append(r.rows, revisions.Revision{Revision: i, ResourceKind: kind})
	}
	return &revisions.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: &fakes.Audit{}, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{}, IDs: &fakes.IDGen{}}, Repo: r}
}

var viewer = acting.Caller{Principal: authz.Principal{ID: "usr_v", Type: authz.PrincipalUser}}

func TestListFiltersAndPages(t *testing.T) {
	s := rig()
	rows, next, err := s.List(t.Context(), viewer, "team_1", revisions.Filter{}, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next != "2" {
		t.Fatalf("page = %v %q %v", rows, next, err)
	}
	rows, _, _ = s.List(t.Context(), viewer, "team_1", revisions.Filter{ResourceKind: "team_tags"}, acting.Page{})
	if len(rows) != 1 || rows[0].Revision != 2 {
		t.Errorf("filtered = %v", rows)
	}
	if rows[0].Snapshot != nil {
		t.Error("list returned a snapshot")
	}
	if _, _, err := s.List(t.Context(), viewer, "team_1", revisions.Filter{}, acting.Page{Cursor: "x"}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad cursor = %v", err)
	}
	if _, _, err := s.List(t.Context(), viewer, "team_2", revisions.Filter{}, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown team = %v", err)
	}
}

func TestGet(t *testing.T) {
	s := rig()
	v, err := s.Get(t.Context(), viewer, "team_1", 2)
	if err != nil || string(v.Snapshot) != "{}" {
		t.Fatalf("get = %+v %v", v, err)
	}
	if _, err := s.Get(t.Context(), viewer, "team_1", 99); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing = %v", err)
	}
	if _, err := s.Get(t.Context(), viewer, "team_2", 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown team = %v", err)
	}
}
