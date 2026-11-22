package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

const secretsTeam = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

// recIdem records the request fingerprint of every claim.
type recIdem struct {
	fakes.Idempotency
	sums [][32]byte
}

func (s *recIdem) Begin(ctx context.Context, tx port.Tx, k port.IdempotencyKey) (*port.StoredResponse, error) {
	s.sums = append(s.sums, k.RequestSHA256)
	return s.Idempotency.Begin(ctx, tx, k)
}

func secretsRig(key []byte) (*SecretsAPI, *recIdem) {
	idem := &recIdem{}
	svc := &secrets.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: &fakes.Audit{}, Outbox: &fakes.Outbox{}, Idem: idem,
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: secretstest.NewRepo(), Writer: secretstest.NewWriter()}
	return &SecretsAPI{Svc: svc, IdemKey: key}, idem
}

func secretReq(t *testing.T, a *SecretsAPI, op, body string) *httptest.ResponseRecorder {
	t.Helper()
	p := authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: secretsTeam}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/teams/"+secretsTeam+"/secrets", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), keyPrincipal, &p))
	r.SetPathValue("teamId", secretsTeam)
	r.SetPathValue("secretId", "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001")
	r.Header.Set("Idempotency-Key", "k-1")
	w := httptest.NewRecorder()
	if op == "rotate" {
		a.rotate(w, r)
	} else {
		a.create(w, r)
	}
	return w
}

// FR-R14 AC3: the stored fingerprint differs between two servers with different keys for one request.
func TestSecretFingerprintDiffersPerServerKey(t *testing.T) {
	body := `{"scope":"team","name":"db-password","value":"hunter2"}`
	a, ia := secretsRig(idemKeyA)
	b, ib := secretsRig(idemKeyB)
	secretReq(t, a, "create", body)
	secretReq(t, b, "create", body)
	if len(ia.sums) != 1 || len(ib.sums) != 1 {
		t.Fatalf("claims: %d %d", len(ia.sums), len(ib.sums))
	}
	if ia.sums[0] == ib.sums[0] {
		t.Fatal("same fingerprint under different server keys")
	}
}

// FR-R14 AC2: a malformed body is 400 and never reaches the idempotency claim, on create and rotate.
func TestSecretMalformedBodyRefusedBeforeIdempotencyClaim(t *testing.T) {
	for _, op := range []string{"create", "rotate"} {
		for _, body := range []string{`not json`, `[]`, `{"value":`, ``} {
			a, idem := secretsRig(idemKeyA)
			w := secretReq(t, a, op, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %q: status %d, want 400", op, body, w.Code)
			}
			if len(idem.sums) != 0 {
				t.Errorf("%s %q: an idempotency claim was made", op, body)
			}
			if strings.Contains(w.Body.String(), "hunter2") {
				t.Errorf("response echoes a value")
			}
		}
	}
}

// A server without a usable key refuses the request instead of hashing the value unkeyed.
func TestSecretWithoutServerKeyIsRefused(t *testing.T) {
	a, idem := secretsRig(nil)
	w := secretReq(t, a, "create", `{"scope":"team","name":"db-password","value":"hunter2"}`)
	if w.Code != http.StatusInternalServerError || len(idem.sums) != 0 {
		t.Fatalf("status %d claims %d", w.Code, len(idem.sums))
	}
	if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "HMAC") {
		t.Fatalf("body leaks detail: %s", w.Body.String())
	}
}
