package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity/identitytest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
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
	p := domain.NewAgentIdentityParams{Kind: domain.IdentityGitHubApp, ExternalRef: "12345", SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"} //nolint:gosec // fixture id
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

// Every repository failure is returned as it is, from every operation that reaches that method: the use
// cases never swallow a storage error or turn it into success.
func TestRepositoryFailuresAreReturned(t *testing.T) {
	boom := errors.New("db down")
	admin := caller(authz.RoleTeamAdmin, team)
	const linkID, idnID = "arl_01JD2Q3R4S5T6V7W8X9Y0Z0001", "aid_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	methods := []string{"Agent", "AccountAllowed", "SecretInTeam", "ListLinks", "GetLink", "InsertLink", "DeleteLink", "RequestVerify",
		"ListIdentities", "GetIdentity", "InsertIdentity", "DeleteIdentity"}
	ops := map[string]func(*rig) error{
		"ListLinks": func(r *rig) error {
			_, _, err := r.svc.ListLinks(t.Context(), admin, team, agent, acting.Page{})
			return err
		},
		"GetLink": func(r *rig) error { _, err := r.svc.GetLink(t.Context(), admin, team, agent, linkID); return err },
		"CreateLink": func(r *rig) error {
			_, err := r.svc.CreateLink(t.Context(), admin, team, agent, domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/new", ProfileName: "new"}, nil)
			return err
		},
		"DeleteLink": func(r *rig) error { return r.svc.DeleteLink(t.Context(), admin, team, agent, linkID, 1) },
		"VerifyLink": func(r *rig) error {
			_, err := r.svc.VerifyLink(t.Context(), admin, team, agent, linkID, nil)
			return err
		},
		"ListIdentities": func(r *rig) error {
			_, _, err := r.svc.ListIdentities(t.Context(), admin, team, agent, acting.Page{})
			return err
		},
		"CreateIdentity": func(r *rig) error {
			_, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, domain.NewAgentIdentityParams{Kind: domain.IdentityExternal, ExternalRef: "x", SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"}, nil) //nolint:gosec // fixture id
			return err
		},
		"DeleteIdentity": func(r *rig) error { return r.svc.DeleteIdentity(t.Context(), admin, team, agent, idnID, 1) },
	}
	for op, call := range ops {
		for _, m := range methods {
			t.Run(op+"/"+m, func(t *testing.T) {
				r := newRig()
				r.repo.Links[linkID] = domain.RoleLink{ID: linkID, TeamID: team, AgentID: agent, AccountID: "111122223333", RoleARN: arn, ProfileName: "prod",
					Status: domain.RoleLinkStatusFailed, Version: 1}
				r.repo.Identities[idnID] = domain.AgentIdentity{ID: idnID, TeamID: team, AgentID: agent, Kind: domain.IdentityGitHubApp, Version: 1}
				if err := call(r); err != nil {
					t.Fatalf("healthy repository: %v", err)
				}
				r = newRig()
				r.repo.Links[linkID] = domain.RoleLink{ID: linkID, TeamID: team, AgentID: agent, AccountID: "111122223333", RoleARN: arn, ProfileName: "prod",
					Status: domain.RoleLinkStatusFailed, Version: 1}
				r.repo.Identities[idnID] = domain.AgentIdentity{ID: idnID, TeamID: team, AgentID: agent, Kind: domain.IdentityGitHubApp, Version: 1}
				r.repo.Fail = map[string]error{m: boom}
				err := call(r)
				if err != nil && !errors.Is(err, boom) {
					t.Errorf("err = %v, want the repository's error", err)
				}
			})
		}
	}
}

type failAudit struct{ *fakes.Audit }

func (failAudit) Write(context.Context, port.Tx, port.AuditRecord) error {
	return errors.New("audit down")
}

type failIDs struct{}

func (failIDs) NewID(domain.IDPrefix) (domain.ID, error) { return "", errors.New("no ids") }

func idemFor(op, key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: team, RequestSHA256: [32]byte{1}},
		Render: func(any) (int, json.RawMessage, error) { return 201, json.RawMessage(`{"ok":true}`), nil }}
}

// The write path stops, and returns the cause, when its audit record, an id, the external id entropy or the
// request's own validation fails; nothing is stored.
func TestWriteFailuresStopTheMutation(t *testing.T) {
	admin := caller(authz.RoleTeamAdmin, team)
	link := domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/new", ProfileName: "new"}
	ident := domain.NewAgentIdentityParams{Kind: domain.IdentityExternal, ExternalRef: "x", SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"} //nolint:gosec // fixture id
	tests := []struct {
		name  string
		setup func(*rig)
		call  func(*rig) error
	}{
		{"link audit", func(r *rig) { r.svc.Audit = failAudit{r.audit} }, func(r *rig) error {
			_, err := r.svc.CreateLink(t.Context(), admin, team, agent, link, nil)
			return err
		}},
		{"link ids", func(r *rig) { r.svc.IDs = failIDs{} }, func(r *rig) error {
			_, err := r.svc.CreateLink(t.Context(), admin, team, agent, link, nil)
			return err
		}},
		{"link entropy", func(r *rig) { r.svc.Entropy = bytes.NewReader(nil) }, func(r *rig) error {
			_, err := r.svc.CreateLink(t.Context(), admin, team, agent, link, nil)
			return err
		}},
		{"identity audit", func(r *rig) { r.svc.Audit = failAudit{r.audit} }, func(r *rig) error {
			_, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, ident, nil)
			return err
		}},
		{"identity ids", func(r *rig) { r.svc.IDs = failIDs{} }, func(r *rig) error {
			_, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, ident, nil)
			return err
		}},
		{"identity invalid", func(*rig) {}, func(r *rig) error {
			_, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, domain.NewAgentIdentityParams{Kind: "nope"}, nil)
			return err
		}},
		{"verify audit", func(r *rig) {
			r.repo.Links["arl_1"] = domain.RoleLink{ID: "arl_1", TeamID: team, AgentID: agent, Status: domain.RoleLinkStatusFailed, Version: 1}
			r.svc.Audit = failAudit{r.audit}
		}, func(r *rig) error {
			_, err := r.svc.VerifyLink(t.Context(), admin, team, agent, "arl_1", nil)
			return err
		}},
		{"delete identity stale", func(r *rig) {
			r.repo.Identities["aid_1"] = domain.AgentIdentity{ID: "aid_1", TeamID: team, AgentID: agent, Kind: domain.IdentityExternal, Version: 2}
		}, func(r *rig) error { return r.svc.DeleteIdentity(t.Context(), admin, team, agent, "aid_1", 1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			tc.setup(r)
			// The in-memory repository is not transactional: the rollback itself is proved by the
			// postgres integration tests.
			if err := tc.call(r); err == nil {
				t.Fatal("the failure was swallowed")
			}
		})
	}
}

// A retry with the same Idempotency-Key replays the stored answer and changes nothing.
func TestCreateIsIdempotent(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	link := domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/new", ProfileName: "new"}
	first, err := r.svc.CreateLink(t.Context(), admin, team, agent, link, idemFor("CreateAgentRoleLink", "k1"))
	if err != nil || first.Replay != nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	again, err := r.svc.CreateLink(t.Context(), admin, team, agent, link, idemFor("CreateAgentRoleLink", "k1"))
	if err != nil || again.Replay == nil || len(r.repo.Links) != 1 {
		t.Fatalf("replay: %+v %v links=%d", again, err, len(r.repo.Links))
	}
	ident := domain.NewAgentIdentityParams{Kind: domain.IdentityExternal, ExternalRef: "x", SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"} //nolint:gosec // fixture id
	if _, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, ident, idemFor("CreateAgentIdentity", "k2")); err != nil {
		t.Fatal(err)
	}
	rep, err := r.svc.CreateIdentity(t.Context(), admin, team, agent, ident, idemFor("CreateAgentIdentity", "k2"))
	if err != nil || rep.Replay == nil || len(r.repo.Identities) != 1 {
		t.Fatalf("identity replay: %+v %v", rep, err)
	}
	for l := range r.repo.Links {
		x := r.repo.Links[l]
		x.Status = domain.RoleLinkStatusFailed
		r.repo.Links[l] = x
		if _, err := r.svc.VerifyLink(t.Context(), admin, team, agent, l, idemFor("VerifyAgentRoleLink", "k3")); err != nil {
			t.Fatal(err)
		}
		if v, err := r.svc.VerifyLink(t.Context(), admin, team, agent, l, idemFor("VerifyAgentRoleLink", "k3")); err != nil || v.Replay == nil {
			t.Fatalf("verify replay: %+v %v", v, err)
		}
	}
}

// Reads need identities:read on the team; another team's caller sees a missing team, never data.
func TestReadsAreAuthorizedAndPaged(t *testing.T) {
	r := newRig()
	for i := range 3 {
		id := fmt.Sprintf("arl_%d", i)
		r.repo.Links[id] = domain.RoleLink{ID: id, TeamID: team, AgentID: agent, Version: 1}
		iid := fmt.Sprintf("aid_%d", i)
		r.repo.Identities[iid] = domain.AgentIdentity{ID: iid, TeamID: team, AgentID: agent, Kind: domain.IdentityExternal, Version: 1}
	}
	viewer := caller(authz.RoleTeamViewer, team)
	rows, next, err := r.svc.ListLinks(t.Context(), viewer, team, agent, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("links page: %d %q %v", len(rows), next, err)
	}
	irows, inext, err := r.svc.ListIdentities(t.Context(), viewer, team, agent, acting.Page{Limit: 2})
	if err != nil || len(irows) != 2 || inext == "" {
		t.Fatalf("identity page: %d %q %v", len(irows), inext, err)
	}
	stranger := caller(authz.RoleTeamAdmin, other)
	if _, err := r.svc.GetLink(t.Context(), stranger, team, agent, "arl_0"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("get link: %v", err)
	}
	if _, _, err := r.svc.ListIdentities(t.Context(), stranger, team, agent, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("list identities: %v", err)
	}
	noRole := acting.Caller{Principal: authz.Principal{ID: "usr_n", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: team}}}}
	if _, err := r.svc.GetLink(t.Context(), noRole, team, agent, "arl_0"); err != nil {
		t.Errorf("viewer get link: %v", err)
	}
}
