package secrets_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const pool = "pool_01JD2Q3R4S5T6V7W8X9Y0Z0001"

func TestCreatePoolSecret(t *testing.T) {
	r := newRig()
	p := params("pool-key")
	p.Scope, p.PoolID = domain.SecretScopePool, pool
	res, err := r.svc.Create(t.Context(), admin, team, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Secret.Scope != domain.SecretScopePool || res.Secret.PoolID != pool || res.Secret.AgentID != "" {
		t.Errorf("secret: %+v", res.Secret)
	}
	tags := r.w.Tags[res.Secret.Path]
	if tags["afs-team-id"] != team || tags["afs-pool-id"] != pool || tags["afs-agent-id"] != "" {
		t.Errorf("tags: %v", tags)
	}
	r.noLeak(t, res)
}

func TestCreatePoolSecretRefusals(t *testing.T) {
	for name, mut := range map[string]func(*domain.NewSecretParams){
		"no pool id":         func(p *domain.NewSecretParams) { p.PoolID = "" },
		"agent id with pool": func(p *domain.NewSecretParams) { p.AgentID = agent },
		"agent prefix":       func(p *domain.NewSecretParams) { p.PoolID = agent },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig()
			p := params("pool-key")
			p.Scope, p.PoolID = domain.SecretScopePool, pool
			mut(&p)
			if _, err := r.svc.Create(t.Context(), admin, team, p, nil); domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("err = %v", err)
			}
			if len(r.w.ByName) != 0 || len(r.audit.Entries) != 0 {
				t.Errorf("a refused create wrote something")
			}
		})
	}
}
