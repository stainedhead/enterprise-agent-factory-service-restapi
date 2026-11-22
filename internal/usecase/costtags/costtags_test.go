package costtags_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type repo struct {
	rows []costtags.Status
	err  error
}

func (r repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]costtags.Status, error) {
	if r.err != nil {
		return nil, r.err
	}
	var out []costtags.Status
	for _, s := range r.rows {
		if s.Key > after {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func rig(rows ...costtags.Status) (*costtags.Service, *fakes.Audit) {
	audit := &fakes.Audit{}
	return &costtags.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: repo{rows: rows}}, audit
}

func caller(p authz.Principal) acting.Caller { return acting.Caller{Principal: p} }

// RA-US-18.1: a service admin lists the keys; everyone else is refused with one denied audit record.
func TestListIsForServiceAdminsOnly(t *testing.T) {
	const teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	now := time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)
	first := now.Add(-48 * time.Hour)
	rows := []costtags.Status{
		{Key: "afs-team-id", SeenInCUR: true, FirstSeenAt: &first, LastSeenAt: &first, AWSStatus: costtags.StatusActive, CheckedAt: now},
		{Key: "business-unit", AWSStatus: costtags.StatusUnknown, CheckedAt: now},
	}
	cases := []struct {
		name string
		p    authz.Principal
		want domain.ErrorKind
	}{
		{"service admin", authz.Principal{ID: "u", ServiceAdmin: true}, ""},
		{"team admin", authz.Principal{ID: "u", Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}}, domain.KindAccessDenied},
		{"team viewer", authz.Principal{ID: "u", Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}}, domain.KindAccessDenied},
		{"security operator", authz.Principal{ID: "u", SecurityOperator: true}, domain.KindAccessDenied},
		{"unbound", authz.Principal{ID: "u"}, domain.KindAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, audit := rig(rows...)
			got, next, err := svc.List(t.Context(), caller(tc.p), acting.Page{})
			if k := domain.KindOf(err); k != tc.want {
				t.Fatalf("kind = %q (%v), want %q", k, err, tc.want)
			}
			if tc.want == "" {
				if len(got) != 2 || next != "" || got[0].Key != "afs-team-id" || !got[0].SeenInCUR || got[1].SeenInCUR {
					t.Errorf("got %+v next %q", got, next)
				}
				if len(audit.Entries) != 0 {
					t.Errorf("a read wrote audit records: %v", audit.Entries)
				}
				return
			}
			if got != nil || len(audit.Entries) != 1 {
				t.Errorf("a refused call returned %v and wrote %d audit records", got, len(audit.Entries))
			}
		})
	}
}

func TestListPaginatesByKey(t *testing.T) {
	var rows []costtags.Status
	for i := 0; i < 5; i++ {
		rows = append(rows, costtags.Status{Key: fmt.Sprintf("k%d", i), AWSStatus: costtags.StatusUnknown})
	}
	svc, _ := rig(rows...)
	admin := caller(authz.Principal{ID: "u", ServiceAdmin: true})
	var seen []string
	cursor := ""
	for range 10 {
		page, next, err := svc.List(t.Context(), admin, acting.Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			seen = append(seen, s.Key)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 || seen[0] != "k0" || seen[4] != "k4" {
		t.Errorf("seen = %v", seen)
	}
}

func TestListRepositoryFailure(t *testing.T) {
	svc, _ := rig()
	svc.Repo = repo{err: errors.New("db down")}
	if _, _, err := svc.List(t.Context(), caller(authz.Principal{ID: "u", ServiceAdmin: true}), acting.Page{}); err == nil {
		t.Error("a repository failure was swallowed")
	}
}
