package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports/importstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type importRig struct {
	h    http.Handler
	repo *importstest.Repo
	out  *fakes.Outbox
}

func newImportRig(t *testing.T) *importRig {
	t.Helper()
	r := &importRig{repo: importstest.New(), out: &fakes.Outbox{}}
	deps := acting.Deps{Tx: &fakes.TxRunner{}, Audit: &fakes.Audit{}, Outbox: r.out, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: w2Now}, IDs: &fakes.IDGen{}}
	api := &ExportsAPI{Imports: &imports.Service{Deps: deps, Repo: r.repo}}
	auth := fakeAuth{
		"svc":     {ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true},
		"unbound": {ID: "usr_u", Type: authz.PrincipalUser},
	}
	h, err := New(Config{Auth: auth, Registrars: []Registrar{api.Register}, Now: func() time.Time { return w2Now }})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	return r
}

func importDoc(t *testing.T, slug string) []byte {
	t.Helper()
	tb := domain.TeamBundle{Team: domain.Row{"slug": slug, "display_name": "payments team", "allowed_aws_accounts": []any{}}, Secrets: []domain.Row{}}
	_, d, err := domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: "2026-11-22T00:00:00Z",
		ExternalSystems: []domain.Row{}, Teams: []domain.TeamBundle{tb}}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func importReq(t *testing.T, bundle []byte) string {
	t.Helper()
	b, err := json.Marshal(map[string]json.RawMessage{"bundle": bundle})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// M4-F: a body over the 100 MB route limit is a ValidationError (the contract has no 413) and nothing is
// read into the use case.
func TestCreateImportOverTheBodyLimit(t *testing.T) {
	r := newImportRig(t)
	big := `{"bundle":{"pad":"` + strings.Repeat("a", (100<<20)+1) + `"}}`
	rec := do(t, r.h, "POST", "/v1/imports", "svc", big)
	if rec.Code != http.StatusBadRequest || errOf(t, rec).Code != "ValidationError" {
		t.Fatalf("status = %d %s, want 400 ValidationError", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	if len(r.repo.Records) != 0 || len(r.out.Events) != 0 {
		t.Error("an oversized import left records")
	}
	// Other routes keep the 1 MiB default: the bundle limit is only for CreateImport.
	if bodyLimits["CreateImport"] != 100<<20 || len(bodyLimits) != 1 {
		t.Errorf("bodyLimits = %v", bodyLimits)
	}
}

// M4-F4: a bundle whose checksum does not verify is refused with a ValidationError and nothing is written,
// for a dry run and for a real import.
func TestCreateImportChecksumMismatch(t *testing.T) {
	r := newImportRig(t)
	good := importDoc(t, "payments")
	tampered := bytes.Replace(good, []byte(`"payments team"`), []byte(`"payments team!"`), 1)
	if bytes.Equal(good, tampered) {
		t.Fatal("the bundle was not tampered")
	}
	for _, dry := range []string{"true", "false"} {
		rec := do(t, r.h, "POST", "/v1/imports?dryRun="+dry, "svc", importReq(t, tampered))
		e := errOf(t, rec)
		if rec.Code != http.StatusBadRequest || e.Code != "ValidationError" || !strings.Contains(strings.ToLower(rec.Body.String()), "bundle.sha256") {
			t.Fatalf("dryRun=%s: %d %s", dry, rec.Code, rec.Body)
		}
	}
	if len(r.repo.Records) != 0 || len(r.out.Events) != 0 {
		t.Error("a tampered bundle wrote records")
	}
	// The untampered bundle imports.
	if rec := do(t, r.h, "POST", "/v1/imports?dryRun=false", "svc", importReq(t, good)); rec.Code != http.StatusOK {
		t.Fatalf("good bundle = %d %s", rec.Code, rec.Body)
	}
}

// Only service admins import (admin:export); everyone else is refused before the body is read.
func TestCreateImportDeniedForNonServiceAdmins(t *testing.T) {
	r := newImportRig(t)
	for _, tok := range []string{"unbound", ""} {
		rec := do(t, r.h, "POST", "/v1/imports", tok, importReq(t, importDoc(t, "payments")))
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401 or 403", tok, rec.Code)
		}
	}
	if len(r.repo.Records) != 0 {
		t.Error("a denied import wrote a record")
	}
}

// An Idempotency-Key replays the first response of a real import and writes nothing the second time; the
// same key with another body is a conflict.
func TestCreateImportIdempotencyReplay(t *testing.T) {
	r := newImportRig(t)
	body := importReq(t, importDoc(t, "payments"))
	first := do(t, r.h, "POST", "/v1/imports?dryRun=false", "svc", body, "Idempotency-Key", "imp-1")
	w2Code(t, first, 200)
	n := len(r.repo.Records)
	events := len(r.out.Events)
	again := do(t, r.h, "POST", "/v1/imports?dryRun=false", "svc", body, "Idempotency-Key", "imp-1")
	w2Code(t, again, 200)
	if again.Header().Get("Idempotent-Replayed") != "true" || !w2SameJSON(again.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("replay = %q %s, first %s", again.Header().Get("Idempotent-Replayed"), again.Body, first.Body)
	}
	if len(r.repo.Records) != n || len(r.out.Events) != events {
		t.Errorf("the replay wrote again: records %d->%d events %d->%d", n, len(r.repo.Records), events, len(r.out.Events))
	}
	other := importReq(t, importDoc(t, "billing"))
	if rec := do(t, r.h, "POST", "/v1/imports?dryRun=false", "svc", other, "Idempotency-Key", "imp-1"); rec.Code != http.StatusConflict {
		t.Errorf("same key, other body = %d %s, want 409", rec.Code, rec.Body)
	}
}
