package identity_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity/identitytest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	agent  = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	shared = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	arn    = "arn:aws:iam::111122223333:role/deployer"
)

type rig struct {
	svc   *identity.Service
	repo  *identitytest.Repo
	audit *fakes.Audit
	out   *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: identitytest.New(), audit: &fakes.Audit{}, out: &fakes.Outbox{}}
	r.svc = &identity.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.out, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}},
		Repo: r.repo, Entropy: bytes.NewReader(bytes.Repeat([]byte{3}, 4096)),
		Roles: func(ns, id string) string { return "arn:aws:iam::999988887777:role/afs/" + ns + "/" + id }}
	r.repo.Agents[agent] = identity.AgentInfo{IAMIdentity: domain.IAMIdentityDedicated, Namespace: "afs-t-pay"}
	r.repo.AgentTeam[agent] = team
	r.repo.Agents[shared] = identity.AgentInfo{IAMIdentity: domain.IAMIdentityTeamShared}
	r.repo.AgentTeam[shared] = team
	r.repo.Allowed[team+"|111122223333"] = true
	r.repo.Secrets["sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"] = team
	return r
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func TestLinkLifecycle(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	p := domain.NewRoleLinkParams{RoleARN: arn, ProfileName: "prod"}
	v, err := r.svc.CreateLink(t.Context(), admin, team, agent, p, nil)
	if err != nil || v.Link.Status != domain.RoleLinkStatusPendingTrust || v.TrustPolicy == nil || v.Link.ExternalID == "" {
		t.Fatalf("%+v %v", v, err)
	}
	if len(r.audit.Entries) != 1 || len(r.out.Events) != 0 {
		t.Fatalf("audit=%d outbox=%d", len(r.audit.Entries), len(r.out.Events))
	}
	if _, err := r.svc.CreateLink(t.Context(), admin, team, agent, p, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := r.svc.GetLink(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, v.Link.ID)
	if err != nil || got.TrustPolicy == nil {
		t.Fatalf("viewer get: %+v %v", got, err)
	}
	rows, _, err := r.svc.ListLinks(t.Context(), admin, team, agent, acting.Page{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	// verify puts the link back to pending_trust
	l := r.repo.Links[v.Link.ID]
	l.Status = domain.RoleLinkStatusFailed
	r.repo.Links[l.ID] = l
	vv, err := r.svc.VerifyLink(t.Context(), admin, team, agent, l.ID, nil)
	if err != nil || vv.Link.Status != domain.RoleLinkStatusPendingTrust {
		t.Fatalf("verify: %+v %v", vv, err)
	}
	l = r.repo.Links[l.ID]
	l.Status = domain.RoleLinkStatusDisabled
	r.repo.Links[l.ID] = l
	if _, err := r.svc.VerifyLink(t.Context(), admin, team, agent, l.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("verify disabled: %v", err)
	}
	if err := r.svc.DeleteLink(t.Context(), admin, team, agent, l.ID, 99); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	if err := r.svc.DeleteLink(t.Context(), admin, team, agent, l.ID, r.repo.Links[l.ID].Version); err != nil || len(r.repo.Links) != 0 {
		t.Fatalf("delete: %v", err)
	}
}

func TestCreateLinkRefusals(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	for name, tc := range map[string]struct {
		agent string
		p     domain.NewRoleLinkParams
	}{
		"shared role":         {shared, domain.NewRoleLinkParams{RoleARN: arn, ProfileName: "p"}},
		"account not allowed": {agent, domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::444455556666:role/x", ProfileName: "p"}},
		"bad arn":             {agent, domain.NewRoleLinkParams{RoleARN: "x", ProfileName: "p"}},
	} {
		if _, err := r.svc.CreateLink(t.Context(), admin, team, tc.agent, tc.p, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.svc.CreateLink(t.Context(), admin, team, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009", domain.NewRoleLinkParams{RoleARN: arn, ProfileName: "p"}, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown agent: %v", err)
	}
}

func TestLinkAuthorization(t *testing.T) {
	r := newRig()
	p := domain.NewRoleLinkParams{RoleARN: arn, ProfileName: "prod"}
	if _, err := r.svc.CreateLink(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, p, nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer create: %v", err)
	}
	if len(r.audit.Entries) != 1 || !r.audit.Entries[0].Own {
		t.Fatalf("denied audit: %d", len(r.audit.Entries))
	}
	if _, err := r.svc.CreateLink(t.Context(), caller(authz.RoleTeamAdmin, other), team, agent, p, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
	if err := r.svc.DeleteLink(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, "arl_x", 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer delete: %v", err)
	}
	if _, err := r.svc.VerifyLink(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, "arl_x", nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer verify: %v", err)
	}
	if _, _, err := r.svc.ListLinks(t.Context(), caller(authz.RoleTeamViewer, other), team, agent, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team list: %v", err)
	}
}

func TestIdentities(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	p := domain.NewAgentIdentityParams{Kind: domain.IdentityGitHubApp, ExternalRef: "12345", SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"}
	res, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, p, nil)
	if err != nil || res.Identity.Status != domain.IdentityPending {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, p, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate kind: %v", err)
	}
	p.Kind, p.SecretID = domain.IdentityExternal, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	if _, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("foreign secret: %v", err)
	}
	if _, err := r.svc.CreateIdentity(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, p, nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer: %v", err)
	}
	rows, _, err := r.svc.ListIdentities(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, acting.Page{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	r.repo.Identities["aid_sa"] = domain.AgentIdentity{ID: "aid_sa", TeamID: team, AgentID: agent, Kind: domain.IdentityK8sServiceAccount, Version: 1}
	if err := r.svc.DeleteIdentity(t.Context(), admin, team, agent, "aid_sa", 1); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("managed: %v", err)
	}
	if err := r.svc.DeleteIdentity(t.Context(), caller(authz.RoleTeamViewer, team), team, agent, res.Identity.ID, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer delete: %v", err)
	}
	if err := r.svc.DeleteIdentity(t.Context(), admin, team, agent, res.Identity.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
