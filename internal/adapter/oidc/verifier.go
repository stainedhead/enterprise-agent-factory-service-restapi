package oidc

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// signatureAlgorithms are the asymmetric algorithms accepted. HMAC and "none" are never accepted: the
// verifier holds no shared secret, and a symmetric algorithm would let anyone who knows a public key
// forge tokens.
var signatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA,
}

// Verifier implements auth.TokenVerifier.
type Verifier struct {
	opts Options
	mu   sync.Mutex
	sets map[string]*keySet
}

// NewVerifier returns a Verifier.
func NewVerifier(opts Options) *Verifier {
	opts.defaults()
	return &Verifier{opts: opts, sets: map[string]*keySet{}}
}

type claims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    *float64        `json:"exp"`
	NotBefore *float64        `json:"nbf"`
	Email     string          `json:"email"`
	Username  string          `json:"preferred_username"`
	UPN       string          `json:"upn"`
	Name      string          `json:"name"`
}

// Verify implements auth.TokenVerifier (RA-US-2.1, RA-US-2.3).
func (v *Verifier) Verify(ctx context.Context, token string, providers []auth.Provider) (auth.Identity, error) {
	jws, err := jose.ParseSignedCompact(token, signatureAlgorithms)
	if err != nil || len(jws.Signatures) != 1 {
		return auth.Identity{}, domain.Unauthenticated()
	}
	// The issuer is read before the signature is checked only to choose which provider's keys to use;
	// nothing in the payload is trusted until the signature verifies.
	var c claims
	if json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &c) != nil || c.Issuer == "" {
		return auth.Identity{}, domain.Unauthenticated()
	}
	cands := candidates(providers, c.Issuer, audiences(c.Audience))
	if len(cands) == 0 {
		return auth.Identity{}, domain.Unauthenticated()
	}
	k, err := v.key(ctx, normalize(cands[0].IssuerURL), jws.Signatures[0].Header.KeyID)
	if err != nil {
		return auth.Identity{}, err
	}
	payload, err := jws.Verify(k)
	if err != nil {
		return auth.Identity{}, domain.Unauthenticated()
	}
	var verified claims
	if json.Unmarshal(payload, &verified) != nil {
		return auth.Identity{}, domain.Unauthenticated()
	}
	if err := v.checkTimes(verified); err != nil {
		return auth.Identity{}, err
	}
	email := firstEmail(verified)
	if verified.Subject == "" || email == "" {
		return auth.Identity{}, domain.Unauthenticated()
	}
	p := cands[0]
	name := verified.Name
	if name == "" {
		name = email
	}
	return auth.Identity{Provider: p, Subject: verified.Subject, Email: email, Name: name, Groups: groups(payload, p.GroupsClaim)}, nil
}

func (v *Verifier) checkTimes(c claims) error {
	now := v.opts.Now()
	if c.Expiry == nil || !now.Before(unix(*c.Expiry).Add(v.opts.Leeway)) {
		return domain.Unauthenticated()
	}
	if c.NotBefore != nil && now.Add(v.opts.Leeway).Before(unix(*c.NotBefore)) {
		return domain.Unauthenticated()
	}
	return nil
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }

// candidates lists the providers whose issuer matches iss and whose client id is among aud, the
// default provider first.
func candidates(providers []auth.Provider, iss string, aud []string) []auth.Provider {
	var out []auth.Provider
	for _, p := range providers {
		if normalize(p.IssuerURL) == normalize(iss) && slices.Contains(aud, p.ClientID) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b auth.Provider) int {
		switch {
		case a.IsDefault && !b.IsDefault:
			return -1
		case !a.IsDefault && b.IsDefault:
			return 1
		}
		return 0
	})
	return out
}

func normalize(issuer string) string { return strings.TrimRight(issuer, "/") }

func audiences(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func firstEmail(c claims) string {
	for _, s := range []string{c.Email, c.Username, c.UPN} {
		if strings.Contains(s, "@") {
			return s
		}
	}
	return ""
}

// groups reads the provider's groups claim from the verified payload: an array of strings, or one
// string.
func groups(payload []byte, claim string) []string {
	var all map[string]json.RawMessage
	if claim == "" || json.Unmarshal(payload, &all) != nil {
		return nil
	}
	raw, ok := all[claim]
	if !ok {
		return nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

var _ auth.TokenVerifier = (*Verifier)(nil)
