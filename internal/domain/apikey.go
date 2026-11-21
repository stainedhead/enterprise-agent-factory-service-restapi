package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"
	"time"
)

// APIKeyPrefix starts every API key secret (API contract §2).
const APIKeyPrefix = "afsk_"

// MaxAPIKeyLifetime is the longest an API key may live (API contract §4.3).
const MaxAPIKeyLifetime = 90 * 24 * time.Hour

// APIKey is a team API key. The secret is never held: only the displayed prefix and the SHA-256 of
// the full key are stored.
type APIKey struct {
	ID         string
	TeamID     string
	Name       string
	Role       TeamRole
	KeyPrefix  string
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
	CreatedBy  string
}

// NewAPIKeyParams is a CreateApiKey body.
type NewAPIKeyParams struct {
	Name      string
	Role      TeamRole
	ExpiresAt time.Time
}

// Validate checks name, role and the 90-day limit against now.
func (p NewAPIKeyParams) Validate(now time.Time) error {
	var fe []FieldError
	if n := len([]rune(strings.TrimSpace(p.Name))); n < 1 || n > 100 {
		fe = append(fe, FieldError{Field: "name", Reason: "must be 1 to 100 characters"})
	}
	if !p.Role.Valid() {
		fe = append(fe, FieldError{Field: "role", Reason: "must be team_admin or team_viewer"})
	}
	switch {
	case p.ExpiresAt.IsZero():
		fe = append(fe, FieldError{Field: "expiresAt", Reason: "is required"})
	case !p.ExpiresAt.After(now):
		fe = append(fe, FieldError{Field: "expiresAt", Reason: "must be in the future"})
	case p.ExpiresAt.After(now.Add(MaxAPIKeyLifetime)):
		fe = append(fe, FieldError{Field: "expiresAt", Reason: "is more than 90 days ahead"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// GeneratedKey is a new key secret with what is stored of it.
type GeneratedKey struct {
	// Secret is the full key, shown once.
	Secret string
	// Prefix is the displayed prefix, for example afsk_3f9a.
	Prefix string
	// SHA256 is the hash of Secret that authentication looks up.
	SHA256 [32]byte
}

// GenerateAPIKey draws 32 random bytes from entropy and encodes the key as afsk_<43 base64url chars>.
func GenerateAPIKey(entropy io.Reader) (GeneratedKey, error) {
	var b [32]byte
	if _, err := io.ReadFull(entropy, b[:]); err != nil {
		return GeneratedKey{}, err
	}
	body := base64.RawURLEncoding.EncodeToString(b[:])
	secret := APIKeyPrefix + body
	return GeneratedKey{Secret: secret, Prefix: APIKeyPrefix + body[:4], SHA256: HashAPIKey(secret)}, nil
}

// HashAPIKey is the SHA-256 of the full key text, the value the database stores and looks up.
func HashAPIKey(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }
