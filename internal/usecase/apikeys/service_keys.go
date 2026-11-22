package apikeys

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ServiceKeyRole is the only role a service-level API key holds (API contract §2, RA-US-15.5).
const ServiceKeyRole domain.TeamRole = domain.TeamRole(authz.RoleSecurityOperator)

const kindServiceKey = "api_keys"

// ServiceRepository is the store of service-level API keys: rows of afs.api_keys with no team.
type ServiceRepository interface {
	// List returns the service keys with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.APIKey, error)
	// Insert stores the key and the hash of its secret; a duplicate name is a Conflict.
	Insert(ctx context.Context, tx port.Tx, k domain.APIKey, sha [32]byte) (domain.APIKey, error)
	// Delete removes the key; NotFound when no service key has the id.
	Delete(ctx context.Context, tx port.Tx, id string) (domain.APIKey, error)
}

// NewServiceKeyParams is a CreateServiceApiKey body.
type NewServiceKeyParams struct {
	Name      string
	Role      string
	ExpiresAt time.Time
}

// Validate checks name, role (security_operator only) and the 90-day limit against now.
func (p NewServiceKeyParams) Validate(now time.Time) error {
	var fe []domain.FieldError
	// The name and expiry rules are those of a team key; the role is checked here.
	if err := (domain.NewAPIKeyParams{Name: p.Name, Role: domain.TeamRoleTeamViewer, ExpiresAt: p.ExpiresAt}).Validate(now); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			fe = append(fe, de.Fields...)
		} else {
			return err
		}
	}
	if p.Role != string(ServiceKeyRole) {
		fe = append(fe, domain.FieldError{Field: "role", Reason: "must be security_operator"})
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// ServiceKeys is the service-level API key use case set (CreateServiceApiKey and its list and delete).
// Only service admins reach it. Like team keys, the secret is returned once and only its SHA-256 is
// stored; the idempotency store never keeps it (D52). Mutations write the row and an audit record;
// contract §7 has no event for them (CD-7).
type ServiceKeys struct {
	acting.Deps
	Repo    ServiceRepository
	Entropy io.Reader
}

func (s *ServiceKeys) guard(ctx context.Context, c acting.Caller, action string) error {
	if authz.Authorize(c.Principal, authz.AdminSettings, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil
	}
	return s.Denied(ctx, c, action, "", authz.AdminSettings)
}

// List implements ListServiceApiKeys.
func (s *ServiceKeys) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.APIKey, string, error) {
	if err := s.guard(ctx, c, "ListServiceApiKeys"); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.APIKey
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(k domain.APIKey) string { return k.ID })
	return page, next, nil
}

// Create implements CreateServiceApiKey.
func (s *ServiceKeys) Create(ctx context.Context, c acting.Caller, in NewServiceKeyParams, idem *acting.Idem) (Created, error) {
	if err := s.guard(ctx, c, "CreateServiceApiKey"); err != nil {
		return Created{}, err
	}
	var out Created
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		out = Created{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			out.Replay = replay
			return err
		}
		if err := in.Validate(s.Now()); err != nil {
			return err
		}
		gen, err := domain.GenerateAPIKey(s.Entropy)
		if err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixAPIKey)
		if err != nil {
			return err
		}
		k, err := s.Repo.Insert(ctx, tx, domain.APIKey{ID: id.String(), Name: in.Name, Role: ServiceKeyRole,
			KeyPrefix: gen.Prefix, ExpiresAt: in.ExpiresAt.UTC()}, gen.SHA256)
		if err != nil {
			return err
		}
		// The audit payload never holds the secret: only id, name, role, prefix and expiry.
		if err := s.Audit.Write(ctx, tx, c.Record("CreateServiceApiKey", kindServiceKey, k.ID, "", nil,
			map[string]any{"name": k.Name, "role": k.Role, "keyPrefix": k.KeyPrefix, "expiresAt": k.ExpiresAt})); err != nil {
			return err
		}
		out = Created{Key: k, Secret: gen.Secret}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, err
}

// Delete implements DeleteServiceApiKey. Keys are immutable, so their version never changes and any
// If-Match the caller sends is accepted.
func (s *ServiceKeys) Delete(ctx context.Context, c acting.Caller, id string) error {
	if err := s.guard(ctx, c, "DeleteServiceApiKey"); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		k, err := s.Repo.Delete(ctx, tx, id)
		if err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteServiceApiKey", kindServiceKey, id, "",
			map[string]any{"name": k.Name, "role": k.Role, "keyPrefix": k.KeyPrefix}, nil))
	})
}
