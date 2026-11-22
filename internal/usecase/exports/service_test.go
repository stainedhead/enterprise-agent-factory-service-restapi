package exports_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports/exportstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func rig() (*exports.Service, *exportstest.Repo, *fakes.Audit, *fakes.Outbox, *fakes.Clock) {
	repo, au, ob, clk := exportstest.New(), &fakes.Audit{}, &fakes.Outbox{}, &fakes.Clock{T: now}
	repo.Teams[teamA] = true
	return &exports.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: au, Outbox: ob, Idem: &fakes.Idempotency{}, Clock: clk, IDs: &fakes.IDGen{}}, Repo: repo}, repo, au, ob, clk
}

var admin = acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}

func TestCreateRecordsPendingExport(t *testing.T) {
	s, repo, au, ob, _ := rig()
	res, err := s.Create(t.Context(), admin, []string{teamA, teamA}, nil)
	if err != nil || res.Export.Status != domain.ExportStatusPending || len(res.Export.TeamIDs) != 1 || res.Export.SchemaVersion != domain.BundleSchemaVersion {
		t.Fatalf("%+v %v", res, err)
	}
	if len(repo.Exports) != 1 || len(au.Entries) != 1 || au.Entries[0].Record.Action != "CreateExport" || len(ob.Events) != 0 {
		t.Fatalf("row %d audit %d events %d", len(repo.Exports), len(au.Entries), len(ob.Events))
	}
}

func TestCreateRefusals(t *testing.T) {
	s, repo, au, _, _ := rig()
	for name, ids := range map[string][]string{"bad id": {"x"}, "unknown team": {"team_01JD2Q3R4S5T6V7W8X9Y0Z0009"}} {
		if _, err := s.Create(t.Context(), admin, ids, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(repo.Exports) != 0 || len(au.Entries) != 0 {
		t.Fatal("a refused request wrote something")
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	s, repo, _, _, _ := rig()
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "CreateExport", RequestSHA256: [32]byte{1}},
			Render: func(v any) (int, json.RawMessage, error) { return 202, json.RawMessage(`{"id":"x"}`), nil }}
	}
	if _, err := s.Create(t.Context(), admin, nil, idem()); err != nil {
		t.Fatal(err)
	}
	res, err := s.Create(t.Context(), admin, nil, idem())
	if err != nil || res.Replay == nil || len(repo.Exports) != 1 {
		t.Fatalf("replay: %+v %v rows %d", res, err, len(repo.Exports))
	}
}

func TestGetAndListHideExpiredLink(t *testing.T) {
	s, repo, _, _, clk := rig()
	url, exp := "https://example.test/x", now.Add(time.Hour)
	repo.Exports["exp_01JD2Q3R4S5T6V7W8X9Y0Z0001"] = domain.Export{ID: "exp_01JD2Q3R4S5T6V7W8X9Y0Z0001", Status: domain.ExportStatusSucceeded, DownloadURL: &url, DownloadExpiresAt: &exp}
	repo.Exports["exp_01JD2Q3R4S5T6V7W8X9Y0Z0002"] = domain.Export{ID: "exp_01JD2Q3R4S5T6V7W8X9Y0Z0002", Status: domain.ExportStatusPending}
	e, err := s.Get(t.Context(), admin, "exp_01JD2Q3R4S5T6V7W8X9Y0Z0001")
	if err != nil || e.DownloadURL == nil {
		t.Fatalf("%+v %v", e, err)
	}
	clk.Advance(2 * time.Hour)
	if e, _ = s.Get(t.Context(), admin, "exp_01JD2Q3R4S5T6V7W8X9Y0Z0001"); e.DownloadURL != nil || e.DownloadExpiresAt == nil {
		t.Fatalf("expired link still served: %+v", e)
	}
	page, next, err := s.List(t.Context(), admin, acting.Page{Limit: 1})
	if err != nil || len(page) != 1 || next == "" || page[0].ID != "exp_01JD2Q3R4S5T6V7W8X9Y0Z0002" {
		t.Fatalf("%+v %q %v", page, next, err)
	}
	page, next, _ = s.List(t.Context(), admin, acting.Page{Limit: 1, Cursor: next})
	if len(page) != 1 || next != "" || page[0].DownloadURL != nil {
		t.Fatalf("%+v %q", page, next)
	}
	for _, id := range []string{"junk", "exp_01JD2Q3R4S5T6V7W8X9Y0Z0009"} {
		if _, err := s.Get(t.Context(), admin, id); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("%s: %v", id, err)
		}
	}
}
