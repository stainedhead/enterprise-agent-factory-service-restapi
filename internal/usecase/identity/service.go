// Package identity implements the agent identity and cross-account role link operations (API contract
// §4.5, AGT-6, AGT-10; M4-A3). A link needs an agent with its own IAM role and an account inside the
// team's allowed-accounts filter; its role ARN and external id never change. The API writes desired
// state only: the links reconciler (reconcile/links) assumes the role and writes status back. Contract
// §7 has no event for these mutations, so each writes the row and an audit record (CD-7).
package identity

import (
	"context"
	"io"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindLink     = "agent_role_links"
	kindIdentity = "agent_identities"
)

// AgentInfo is what the use case needs to know of an agent.
type AgentInfo struct {
	// IAMIdentity is team_shared or dedicated.
	IAMIdentity domain.IAMIdentity
	// Namespace is the team's namespace (afs-t-<slug>).
	Namespace string
	// RoleARN is the agent's IAM role as the reconciler recorded it; empty until then.
	RoleARN string
}

// Repository is the agent_role_links and agent_identities store.
type Repository interface {
	// Agent returns the live agent of the team; NotFound otherwise.
	Agent(ctx context.Context, tx port.Tx, teamID, agentID string) (AgentInfo, error)
	// AccountAllowed reports afs.account_allowed(team, account).
	AccountAllowed(ctx context.Context, tx port.Tx, teamID, accountID string) (bool, error)
	// SecretInTeam reports whether the team owns the secret.
	SecretInTeam(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error)

	ListLinks(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.RoleLink, error)
	GetLink(ctx context.Context, tx port.Tx, teamID, agentID, linkID string, forUpdate bool) (domain.RoleLink, error)
	// InsertLink stores a link; a duplicate role or profile of the agent is a Conflict.
	InsertLink(ctx context.Context, tx port.Tx, l domain.RoleLink) error
	// DeleteLink removes a link; PreconditionFailed when the version is not expect.
	DeleteLink(ctx context.Context, tx port.Tx, teamID, agentID, linkID string, expect domain.Version) error
	// RequestVerify sets the link back to pending_trust (clearing the last error) so the reconciler
	// verifies it again, and returns the stored link.
	RequestVerify(ctx context.Context, tx port.Tx, teamID, agentID, linkID string) (domain.RoleLink, error)

	ListIdentities(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.AgentIdentity, error)
	GetIdentity(ctx context.Context, tx port.Tx, teamID, agentID, id string, forUpdate bool) (domain.AgentIdentity, error)
	// InsertIdentity stores an identity; a second one of the same kind for the agent is a Conflict.
	InsertIdentity(ctx context.Context, tx port.Tx, i domain.AgentIdentity) error
	DeleteIdentity(ctx context.Context, tx port.Tx, teamID, agentID, id string, expect domain.Version) error
}

// RoleARNs computes the ARN of a dedicated agent role; nil means the ARN is only known once recorded.
type RoleARNs func(namespace, agentID string) string

// Service is the identity use case set.
type Service struct {
	acting.Deps
	Repo    Repository
	Roles   RoleARNs
	Entropy io.Reader
}

// View is a link with its trust policy (set by GetAgentRoleLink and the writes).
type View struct {
	Link        domain.RoleLink
	TrustPolicy map[string]any
	Replay      *port.StoredResponse
}

// CreatedIdentity is an identity with the replayed response of an idempotent create.
type CreatedIdentity struct {
	Identity domain.AgentIdentity
	Replay   *port.StoredResponse
}

func (s *Service) check(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

func (s *Service) trust(info AgentInfo, agentID string, l domain.RoleLink) map[string]any {
	arn := info.RoleARN
	if arn == "" && s.Roles != nil && info.IAMIdentity == domain.IAMIdentityDedicated {
		arn = s.Roles(info.Namespace, agentID)
	}
	return l.TrustPolicy(arn)
}

// ListLinks implements ListAgentRoleLinks.
func (s *Service) ListLinks(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.RoleLink, string, error) {
	if err := s.check(ctx, c, "ListAgentRoleLinks", teamID, authz.IdentitiesRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.RoleLink
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Agent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.ListLinks(ctx, tx, teamID, agentID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(l domain.RoleLink) string { return l.ID })
	return page, next, nil
}

// GetLink implements GetAgentRoleLink, with the trust policy to give the target account's owner.
func (s *Service) GetLink(ctx context.Context, c acting.Caller, teamID, agentID, linkID string) (View, error) {
	if err := s.check(ctx, c, "GetAgentRoleLink", teamID, authz.IdentitiesRead); err != nil {
		return View{}, err
	}
	var v View
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		info, err := s.Repo.Agent(ctx, tx, teamID, agentID)
		if err != nil {
			return err
		}
		if v.Link, err = s.Repo.GetLink(ctx, tx, teamID, agentID, linkID, false); err != nil {
			return err
		}
		v.TrustPolicy = s.trust(info, agentID, v.Link)
		return nil
	})
	return v, err
}

// CreateLink implements CreateAgentRoleLink: the agent must have a dedicated role and the account must be
// active and inside the team's allowed-accounts filter (both 400 otherwise); the link starts
// pending_trust and the reconciler verifies it once the target account trusts the agent's role.
func (s *Service) CreateLink(ctx context.Context, c acting.Caller, teamID, agentID string, in domain.NewRoleLinkParams, idem *acting.Idem) (View, error) {
	var res View
	if err := s.check(ctx, c, "CreateAgentRoleLink", teamID, authz.IdentitiesWrite); err != nil {
		return res, err
	}
	if err := in.Validate(); err != nil {
		return res, err
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = View{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		info, err := s.Repo.Agent(ctx, tx, teamID, agentID)
		if err != nil {
			return err
		}
		if info.IAMIdentity != domain.IAMIdentityDedicated {
			return domain.Validation(domain.FieldError{Field: "agentId", Reason: "the agent needs its own IAM role (iamIdentity dedicated) before it can assume roles in other accounts"})
		}
		account := domain.AccountOfRoleARN(in.RoleARN)
		if ok, err := s.Repo.AccountAllowed(ctx, tx, teamID, account); err != nil {
			return err
		} else if !ok {
			return domain.Validation(domain.FieldError{Field: "roleArn", Reason: "account " + account + " is not an active account in this team's allowed accounts"})
		}
		id, err := s.IDs.NewID(domain.PrefixRoleLink)
		if err != nil {
			return err
		}
		ext, err := domain.NewExternalID(s.Entropy)
		if err != nil {
			return err
		}
		l, err := domain.NewRoleLink(id.String(), teamID, agentID, ext, in, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.InsertLink(ctx, tx, l); err != nil {
			return err
		}
		if l, err = s.Repo.GetLink(ctx, tx, teamID, agentID, l.ID, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgentRoleLink", kindLink, l.ID, teamID, nil, l.Audit())); err != nil {
			return err
		}
		res = View{Link: l, TrustPolicy: s.trust(info, agentID, l)}
		return idem.Complete(ctx, s.Idem, tx, res)
	})
	return res, err
}

// DeleteLink implements DeleteAgentRoleLink (If-Match required).
func (s *Service) DeleteLink(ctx context.Context, c acting.Caller, teamID, agentID, linkID string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteAgentRoleLink", teamID, authz.IdentitiesWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Agent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		cur, err := s.Repo.GetLink(ctx, tx, teamID, agentID, linkID, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteLink(ctx, tx, teamID, agentID, linkID, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteAgentRoleLink", kindLink, linkID, teamID, cur.Audit(), nil))
	})
}

// VerifyLink implements VerifyAgentRoleLink: it puts the link back to pending_trust so the reconciler
// verifies it on its next pass (202). A disabled link is a Conflict: widen the allowed accounts first.
func (s *Service) VerifyLink(ctx context.Context, c acting.Caller, teamID, agentID, linkID string, idem *acting.Idem) (View, error) {
	var res View
	if err := s.check(ctx, c, "VerifyAgentRoleLink", teamID, authz.IdentitiesWrite); err != nil {
		return res, err
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = View{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		info, err := s.Repo.Agent(ctx, tx, teamID, agentID)
		if err != nil {
			return err
		}
		cur, err := s.Repo.GetLink(ctx, tx, teamID, agentID, linkID, true)
		if err != nil {
			return err
		}
		if cur.Status == domain.RoleLinkStatusDisabled {
			return domain.Conflict("link_disabled", "the link is disabled because its account is outside the team's allowed accounts", nil)
		}
		l, err := s.Repo.RequestVerify(ctx, tx, teamID, agentID, linkID)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("VerifyAgentRoleLink", kindLink, linkID, teamID, cur.Audit(), l.Audit())); err != nil {
			return err
		}
		res = View{Link: l, TrustPolicy: s.trust(info, agentID, l)}
		return idem.Complete(ctx, s.Idem, tx, res)
	})
	return res, err
}

// ListIdentities implements ListAgentIdentities.
func (s *Service) ListIdentities(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.AgentIdentity, string, error) {
	if err := s.check(ctx, c, "ListAgentIdentities", teamID, authz.IdentitiesRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.AgentIdentity
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Agent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.ListIdentities(ctx, tx, teamID, agentID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(i domain.AgentIdentity) string { return i.ID })
	return page, next, nil
}

// CreateIdentity implements CreateAgentIdentity (github_app and external kinds). A referenced secret
// must belong to the team.
func (s *Service) CreateIdentity(ctx context.Context, c acting.Caller, teamID, agentID string, in domain.NewAgentIdentityParams, idem *acting.Idem) (CreatedIdentity, error) {
	var res CreatedIdentity
	if err := s.check(ctx, c, "CreateAgentIdentity", teamID, authz.IdentitiesWrite); err != nil {
		return res, err
	}
	if err := in.Validate(); err != nil {
		return res, err
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = CreatedIdentity{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if _, err := s.Repo.Agent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		if in.SecretID != "" {
			if ok, err := s.Repo.SecretInTeam(ctx, tx, teamID, in.SecretID); err != nil {
				return err
			} else if !ok {
				return domain.Validation(domain.FieldError{Field: "secretId", Reason: "is not a secret of this team"})
			}
		}
		id, err := s.IDs.NewID(domain.PrefixAgentIdentity)
		if err != nil {
			return err
		}
		ident, err := domain.NewAgentIdentity(id.String(), teamID, agentID, in, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.InsertIdentity(ctx, tx, ident); err != nil {
			return err
		}
		if ident, err = s.Repo.GetIdentity(ctx, tx, teamID, agentID, ident.ID, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgentIdentity", kindIdentity, ident.ID, teamID, nil, ident.Audit())); err != nil {
			return err
		}
		res.Identity = ident
		return idem.Complete(ctx, s.Idem, tx, ident)
	})
	return res, err
}

// DeleteIdentity implements DeleteAgentIdentity (If-Match required). The identities AFS manages (the
// ServiceAccount and the IAM role) cannot be deleted: they follow the agent.
func (s *Service) DeleteIdentity(ctx context.Context, c acting.Caller, teamID, agentID, id string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteAgentIdentity", teamID, authz.IdentitiesWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Agent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		cur, err := s.Repo.GetIdentity(ctx, tx, teamID, agentID, id, true)
		if err != nil {
			return err
		}
		if !cur.Kind.Creatable() {
			return domain.Conflict("managed_identity", "the "+string(cur.Kind)+" identity is managed by AFS and goes with the agent", nil)
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteIdentity(ctx, tx, teamID, agentID, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteAgentIdentity", kindIdentity, id, teamID, cur.Audit(), nil))
	})
}
