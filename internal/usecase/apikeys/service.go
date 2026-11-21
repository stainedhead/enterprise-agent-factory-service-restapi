// Package apikeys implements ListApiKeys, CreateApiKey and DeleteApiKey (API contract §4.3). The
// secret is returned once and only its SHA-256 is stored; the idempotency store never keeps it
// (D52). Mutations write the row and an audit record; contract §7 has no API key event, so no
// outbox row is written (CD-7).
package apikeys

import (
	"context"
	"io"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kindKey = "api_keys"

// Repository is the team API key store.
type Repository interface {
	// List returns the team's keys with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.APIKey, error)
	// Insert stores the key and the hash of its secret; a duplicate name is a Conflict.
	Insert(ctx context.Context, tx port.Tx, k domain.APIKey, sha [32]byte) (domain.APIKey, error)
	// Delete removes the key; NotFound when the team has no such key.
	Delete(ctx context.Context, tx port.Tx, teamID, id string) (domain.APIKey, error)
}

// Service is the API key use case set.
type Service struct {
	acting.Deps
	Repo    Repository
	Entropy io.Reader
}

// Created is a new key with its one-time secret, or the replayed response of an idempotent create
// (which has no secret).
type Created struct {
	Key    domain.APIKey
	Secret string
	Replay *port.StoredResponse
}

// List implements ListApiKeys.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.APIKey, string, error) {
	p = p.Normalized()
	var rows []domain.APIKey
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(k domain.APIKey) string { return k.ID })
	return page, next, nil
}

// Create implements CreateApiKey.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewAPIKeyParams, idem *acting.Idem) (Created, error) {
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
		k, err := s.Repo.Insert(ctx, tx, domain.APIKey{ID: id.String(), TeamID: teamID, Name: in.Name, Role: in.Role,
			KeyPrefix: gen.Prefix, ExpiresAt: in.ExpiresAt.UTC()}, gen.SHA256)
		if err != nil {
			return err
		}
		// The audit payload never holds the secret: only id, name, role, prefix and expiry.
		if err := s.Audit.Write(ctx, tx, c.Record("CreateApiKey", kindKey, k.ID, teamID, nil,
			map[string]any{"name": k.Name, "role": k.Role, "keyPrefix": k.KeyPrefix, "expiresAt": k.ExpiresAt})); err != nil {
			return err
		}
		out = Created{Key: k, Secret: gen.Secret}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, err
}

// Delete implements DeleteApiKey. Keys are immutable, so their version never changes and any
// If-Match the caller sends is accepted.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string) error {
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		k, err := s.Repo.Delete(ctx, tx, teamID, id)
		if err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteApiKey", kindKey, id, teamID,
			map[string]any{"name": k.Name, "role": k.Role, "keyPrefix": k.KeyPrefix}, nil))
	})
}
