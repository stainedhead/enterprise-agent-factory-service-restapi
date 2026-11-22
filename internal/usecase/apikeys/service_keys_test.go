package apikeys_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys/apikeystest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

func srig() (*apikeys.ServiceKeys, *apikeystest.ServiceRepo, *fakes.Audit, *fakes.Outbox, *fakes.Idempotency) {
	repo, audit, outbox, idem := apikeystest.NewService(), &fakes.Audit{}, &fakes.Outbox{}, &fakes.Idempotency{}
	return &apikeys.ServiceKeys{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: outbox, Idem: idem,
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: repo, Entropy: bytes.NewReader(bytes.Repeat([]byte{7}, 1024))}, repo, audit, outbox, idem
}

var serviceAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_sa", ServiceAdmin: true}}

func sparams(name string) apikeys.NewServiceKeyParams {
	return apikeys.NewServiceKeyParams{Name: name, Role: "security_operator", ExpiresAt: now.Add(30 * 24 * time.Hour)}
}

func TestServiceKeyShowsSecretOnceAndStoresHash(t *testing.T) {
	svc, repo, audit, outbox, _ := srig()
	res, err := svc.Create(t.Context(), serviceAdmin, sparams("scanner"), nil)
	if err != nil || !strings.HasPrefix(res.Secret, "afsk_") {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Key.TeamID != "" || res.Key.Role != apikeys.ServiceKeyRole {
		t.Fatalf("a service key has no team and the security_operator role: %+v", res.Key)
	}
	if repo.Hashes[res.Key.ID] != domain.HashAPIKey(res.Secret) {
		t.Fatal("stored hash is not the SHA-256 of the secret")
	}
	if len(audit.Entries) != 1 || audit.Entries[0].Record.TeamID != "" || len(outbox.Events) != 0 {
		t.Fatalf("audit=%+v outbox=%d", audit.Entries, len(outbox.Events))
	}
	if b, _ := json.Marshal(audit.Entries[0].Record); strings.Contains(string(b), res.Secret) {
		t.Fatal("secret in audit record")
	}
	rows, _, err := svc.List(t.Context(), serviceAdmin, acting.Page{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if b, _ := json.Marshal(rows); strings.Contains(string(b), res.Secret) {
		t.Fatal("secret in the list")
	}
	if _, err := svc.Create(t.Context(), serviceAdmin, sparams("scanner"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestServiceKeyValidation(t *testing.T) {
	svc, _, _, _, _ := srig()
	for name, mut := range map[string]func(*apikeys.NewServiceKeyParams){
		"team role":    func(p *apikeys.NewServiceKeyParams) { p.Role = "team_admin" },
		"service role": func(p *apikeys.NewServiceKeyParams) { p.Role = "service_admin" },
		"no role":      func(p *apikeys.NewServiceKeyParams) { p.Role = "" },
		"no name":      func(p *apikeys.NewServiceKeyParams) { p.Name = "" },
		"past expiry":  func(p *apikeys.NewServiceKeyParams) { p.ExpiresAt = now.Add(-time.Hour) },
		"91 days":      func(p *apikeys.NewServiceKeyParams) { p.ExpiresAt = now.Add(91 * 24 * time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			p := sparams("ok")
			mut(&p)
			if _, err := svc.Create(t.Context(), serviceAdmin, p, nil); domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestServiceKeysAreForServiceAdminsOnly(t *testing.T) {
	svc, repo, audit, _, _ := srig()
	made, _ := svc.Create(t.Context(), serviceAdmin, sparams("scanner"), nil)
	callers := map[string]acting.Caller{
		"team admin":        admin,
		"security operator": {Principal: authz.Principal{ID: "usr_so", SecurityOperator: true}},
		"service key":       {Principal: authz.Principal{ID: "key_x", Type: authz.PrincipalAPIKey, SecurityOperator: true}},
		"workload":          {Principal: authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: team, PoolID: "pool_1"}}},
	}
	for name, c := range callers {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Create(t.Context(), c, sparams("evil"), nil); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("create: %v", err)
			}
			if _, _, err := svc.List(t.Context(), c, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("list: %v", err)
			}
			if err := svc.Delete(t.Context(), c, made.Key.ID); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("delete: %v", err)
			}
		})
	}
	if len(repo.Keys) != 1 {
		t.Fatalf("a refused call changed the keys: %v", repo.Keys)
	}
	denied := 0
	for _, e := range audit.Entries {
		if e.Record.Outcome == port.OutcomeDenied {
			denied++
		}
	}
	if denied != 12 {
		t.Errorf("denied audit records = %d, want 12", denied)
	}
	if err := svc.Delete(t.Context(), serviceAdmin, made.Key.ID); err != nil || len(repo.Keys) != 0 {
		t.Fatalf("%v", err)
	}
	if err := svc.Delete(t.Context(), serviceAdmin, made.Key.ID); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("gone: %v", err)
	}
}

func TestServiceKeyReplayHasNoSecret(t *testing.T) {
	svc, repo, _, _, _ := srig()
	key := port.IdempotencyKey{Key: "k1", Operation: "CreateServiceApiKey", RequestSHA256: [32]byte{2}}
	first, err := svc.Create(t.Context(), serviceAdmin, sparams("scanner"), &acting.Idem{Key: key, Render: render})
	if err != nil || first.Secret == "" {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := svc.Create(t.Context(), serviceAdmin, sparams("scanner"), &acting.Idem{Key: key, Render: render})
	if err != nil || again.Replay == nil || again.Secret != "" || len(repo.Keys) != 1 {
		t.Fatalf("%+v %v", again, err)
	}
	if strings.Contains(string(again.Replay.Body), "secretKey") || strings.Contains(string(again.Replay.Body), first.Secret) {
		t.Fatalf("replay body carries the secret: %s", again.Replay.Body)
	}
}
