// Package idp implements the identity provider administration operations (API contract §4.11).
// A provider never holds a client secret, only its Secrets Manager ARN. Contract §7 has no provider
// event, so a change writes the row and an audit record only (CD-7).
package idp

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "identity_providers"

// TeamRef is a team using a provider.
type TeamRef struct{ ID, Slug, DisplayName string }

// Repository is the identity_providers store.
type Repository interface {
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.IdentityProvider, error)
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.IdentityProvider, error)
	// TeamsUsing lists the non-deleted teams whose identityProviderId is id.
	TeamsUsing(ctx context.Context, tx port.Tx, id string) ([]TeamRef, error)
	// ClearDefault unsets isDefault on every provider except id.
	ClearDefault(ctx context.Context, tx port.Tx, exceptID string) error
	Insert(ctx context.Context, tx port.Tx, p domain.IdentityProvider) error
	// Update writes the mutable fields; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, p domain.IdentityProvider, expect domain.Version) (domain.IdentityProvider, error)
	Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
}

// Service is the identity provider use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Patch is an UpdateIdentityProvider body; nil means absent.
type Patch struct {
	Name            *string
	ConsoleClientID *string
	CLIClientID     *string
	ClientSecretARN *domain.OptionalString
	GroupsClaim     *string
	IsDefault       *bool
	Enabled         *bool
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string) error {
	if !authz.Authorize(c.Principal, authz.AdminIdentityProviders, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", authz.AdminIdentityProviders)
	}
	return nil
}

func audit(p domain.IdentityProvider) map[string]any {
	return map[string]any{"kind": p.Kind, "name": p.Name, "issuerUrl": p.IssuerURL, "consoleClientId": p.ConsoleClientID,
		"cliClientId": p.CLIClientID, "clientSecretArn": p.ClientSecretARN, "groupsClaim": p.GroupsClaim,
		"isDefault": p.IsDefault, "enabled": p.Enabled}
}

// List implements ListIdentityProviders.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.IdentityProvider, string, error) {
	if err := s.check(ctx, c, "ListIdentityProviders"); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.IdentityProvider
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.IdentityProvider) string { return x.ID })
	return page, next, nil
}

// Get implements GetIdentityProvider: the provider and the teams using it.
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (domain.IdentityProvider, []TeamRef, error) {
	if err := s.check(ctx, c, "GetIdentityProvider"); err != nil {
		return domain.IdentityProvider{}, nil, err
	}
	var (
		p     domain.IdentityProvider
		teams []TeamRef
	)
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		if p, err = s.Repo.Get(ctx, tx, id, false); err != nil {
			return err
		}
		teams, err = s.Repo.TeamsUsing(ctx, tx, id)
		return err
	})
	return p, teams, err
}

// Create implements CreateIdentityProvider; a new default replaces the previous one.
func (s *Service) Create(ctx context.Context, c acting.Caller, in domain.IdentityProvider, idem *acting.Idem) (domain.IdentityProvider, *port.StoredResponse, error) {
	if err := s.check(ctx, c, "CreateIdentityProvider"); err != nil {
		return domain.IdentityProvider{}, nil, err
	}
	var (
		out    domain.IdentityProvider
		replay *port.StoredResponse
	)
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, replay = domain.IdentityProvider{}, nil
		if replay, err = idem.Begin(ctx, s.Idem, tx); err != nil || replay != nil {
			return err
		}
		if in.GroupsClaim == "" {
			in.GroupsClaim = "groups"
		}
		if err := in.Validate(); err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixIdP)
		if err != nil {
			return err
		}
		in.ID = id.String()
		if in.IsDefault {
			if err := s.Repo.ClearDefault(ctx, tx, in.ID); err != nil {
				return err
			}
		}
		if err := s.Repo.Insert(ctx, tx, in); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateIdentityProvider", kind, in.ID, "", nil, audit(in))); err != nil {
			return err
		}
		if out, err = s.Repo.Get(ctx, tx, in.ID, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, replay, err
}

// Update implements UpdateIdentityProvider: disabling a provider that teams use is a Conflict.
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, expect domain.Version, patch Patch) (domain.IdentityProvider, error) {
	if err := s.check(ctx, c, "UpdateIdentityProvider"); err != nil {
		return domain.IdentityProvider{}, err
	}
	var out domain.IdentityProvider
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		n := cur
		if patch.Name != nil {
			n.Name = *patch.Name
		}
		if patch.ConsoleClientID != nil {
			n.ConsoleClientID = *patch.ConsoleClientID
		}
		if patch.CLIClientID != nil {
			n.CLIClientID = *patch.CLIClientID
		}
		if patch.ClientSecretARN != nil {
			n.ClientSecretARN = ""
			if patch.ClientSecretARN.Value != nil {
				n.ClientSecretARN = *patch.ClientSecretARN.Value
			}
		}
		if patch.GroupsClaim != nil {
			n.GroupsClaim = *patch.GroupsClaim
		}
		if patch.IsDefault != nil {
			n.IsDefault = *patch.IsDefault
		}
		if patch.Enabled != nil {
			n.Enabled = *patch.Enabled
		}
		if err := n.Validate(); err != nil {
			return err
		}
		if cur.Enabled && !n.Enabled {
			if err := s.refuseIfUsed(ctx, tx, id, "a provider that teams use cannot be disabled"); err != nil {
				return err
			}
		}
		if n.IsDefault && !cur.IsDefault {
			if err := s.Repo.ClearDefault(ctx, tx, id); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, n, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateIdentityProvider", kind, id, "", audit(cur), audit(out)))
	})
	return out, err
}

func (s *Service) refuseIfUsed(ctx context.Context, tx port.Tx, id, msg string) error {
	teams, err := s.Repo.TeamsUsing(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(teams) == 0 {
		return nil
	}
	ids := make([]string, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	return domain.Conflict("in_use", msg, map[string]any{"dependents": ids})
}

// Delete implements DeleteIdentityProvider: a provider that teams use is a Conflict.
func (s *Service) Delete(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteIdentityProvider"); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.refuseIfUsed(ctx, tx, id, "a provider that teams use cannot be deleted"); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteIdentityProvider", kind, id, "", audit(cur), nil))
	})
}
