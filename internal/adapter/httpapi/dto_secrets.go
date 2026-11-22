package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// secretBody is the wire shape of Secret (API contract §5). It has no value field, ever.
type secretBody struct {
	ID                 string         `json:"id"`
	Version            int64          `json:"version"`
	CreatedAt          string         `json:"createdAt"`
	CreatedBy          string         `json:"createdBy"`
	UpdatedAt          string         `json:"updatedAt"`
	Scope              string         `json:"scope"`
	AgentID            string         `json:"agentId,omitempty"`
	PoolID             string         `json:"poolId,omitempty"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	ExternalSystemID   *string        `json:"externalSystemId"`
	SecretsManagerPath string         `json:"secretsManagerPath"`
	SecretsManagerArn  string         `json:"secretsManagerArn"`
	VersionID          string         `json:"versionId,omitempty"`
	LastChangedAt      *string        `json:"lastChangedAt"`
	Rotation           secretRotation `json:"rotation"`
	BoundTo            []string       `json:"boundTo"`
}

type secretRotation struct {
	Enabled bool `json:"enabled"`
	Days    int  `json:"days,omitempty"`
}

func toSecretBody(s domain.Secret) secretBody {
	b := secretBody{ID: s.ID, Version: int64(s.Version), CreatedAt: w2Time(s.CreatedAt), CreatedBy: s.CreatedBy,
		UpdatedAt: w2Time(s.UpdatedAt), Scope: string(s.Scope), AgentID: s.AgentID, PoolID: s.PoolID, Name: s.Name,
		Description: s.Description, SecretsManagerPath: s.Path, SecretsManagerArn: s.ARN, VersionID: s.VersionID,
		LastChangedAt: w2TimePtr(s.LastChangedAt), Rotation: secretRotation{Enabled: s.Rotation.Enabled, Days: s.Rotation.Days},
		BoundTo: s.BoundTo}
	if s.ExternalSystemID != "" {
		x := s.ExternalSystemID
		b.ExternalSystemID = &x
	}
	if b.BoundTo == nil {
		b.BoundTo = []string{}
	}
	return b
}

type rotationRequest struct {
	Enabled *bool `json:"enabled"`
	Days    *int  `json:"days"`
}

func (q *rotationRequest) domain(field string) (domain.Rotation, error) {
	if q == nil {
		return domain.Rotation{}, nil
	}
	if q.Enabled == nil {
		return domain.Rotation{}, bad(field+".enabled", "is required")
	}
	r := domain.Rotation{Enabled: *q.Enabled}
	if q.Days != nil {
		r.Days = *q.Days
	}
	return r, r.Validate()
}

// createSecretRequest, putSecretValueRequest and rotateSecretRequest are the only DTOs that hold a
// secret value, as a plain string until it is turned into a domain.SecretValue.
type createSecretRequest struct {
	Scope            string           `json:"scope"`
	AgentID          string           `json:"agentId"`
	PoolID           string           `json:"poolId"`
	Name             string           `json:"name"`
	Description      string           `json:"description"`
	ExternalSystemID string           `json:"externalSystemId"`
	Rotation         *rotationRequest `json:"rotation"`
	Value            *string          `json:"value"`
}

type valueRequest struct {
	Value *string `json:"value"`
}

type updateSecretRequest struct {
	Description      *string            `json:"description"`
	ExternalSystemID w2Nullable[string] `json:"externalSystemId"`
	Rotation         *rotationRequest   `json:"rotation"`
}

// decodeSecretBody decodes a secret route's body. Unlike Decode it never repeats the parser's
// message, which can quote a character of the body, and so cannot echo part of a value.
func decodeSecretBody(r *http.Request, dst any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return bad("body", "request body is larger than the limit")
		}
		return bad("body", "unreadable request body")
	}
	if pathTeam := r.PathValue("teamId"); pathTeam != "" {
		var probe struct {
			TeamID *string `json:"teamId"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.TeamID != nil && *probe.TeamID != pathTeam {
			return bad("teamId", "must match the team in the path")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return bad(te.Field, "has the wrong type")
		}
		return bad("body", "must be a JSON object with the documented fields")
	}
	if dec.More() {
		return bad("body", "trailing data after the JSON document")
	}
	return nil
}

// idemBody is the body hashed for idempotency. The value is replaced by a 32-bit tag of itself, so
// a reuse of the key with another value is still a mismatch (409) while the hash stored in the
// database reveals at most 32 bits of the value and cannot be used to guess a secret.
func idemBody(raw []byte, value *string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	delete(m, "value")
	if value != nil {
		sum := sha256.Sum256([]byte("afs-idem-value|" + *value))
		m["valueTag"] = json.RawMessage(strconv.Quote(hex.EncodeToString(sum[:4])))
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

func secretValue(v *string) (domain.SecretValue, error) {
	if v == nil {
		return domain.SecretValue{}, bad("value", "is required")
	}
	return domain.NewSecretValue("value", *v)
}
