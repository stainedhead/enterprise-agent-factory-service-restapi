package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// idp is a fake identity provider: discovery, a swappable JWKS and a token signer.
type idp struct {
	srv      *httptest.Server
	mu       sync.Mutex
	keys     map[string]crypto // kid -> key
	down     bool
	jwksHits atomic.Int64
}

type crypto struct {
	priv any
	alg  jose.SignatureAlgorithm
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{keys: map[string]crypto{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		p.jwksHits.Add(1)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var set jose.JSONWebKeySet
		for kid, k := range p.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: pub(k.priv), KeyID: kid, Algorithm: string(k.alg), Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func pub(priv any) any {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	}
	return nil
}

func (p *idp) addRSA(t *testing.T, kid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys[kid] = crypto{k, jose.RS256}
	p.mu.Unlock()
}

func (p *idp) addEC(t *testing.T, kid string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys[kid] = crypto{k, jose.ES256}
	p.mu.Unlock()
}

func (p *idp) remove(kid string) {
	p.mu.Lock()
	delete(p.keys, kid)
	p.mu.Unlock()
}

func (p *idp) setDown(d bool) {
	p.mu.Lock()
	p.down = d
	p.mu.Unlock()
}

func (p *idp) sign(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	p.mu.Lock()
	k := p.keys[kid]
	p.mu.Unlock()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: k.alg, Key: k.priv}, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var t0 = time.Unix(1_800_000_000, 0)

func setup(t *testing.T) (*idp, *Verifier, *clock, auth.Provider) {
	t.Helper()
	p := newIDP(t)
	p.addRSA(t, "k1")
	ck := &clock{t: t0}
	v := NewVerifier(Options{HTTP: p.srv.Client(), Now: ck.now})
	prov := auth.Provider{ID: "idp_1", Kind: "okta", Name: "Okta", IssuerURL: p.srv.URL, ClientID: "client-1", GroupsClaim: "groups", IsDefault: true}
	return p, v, ck, prov
}

func good(p *idp, extra map[string]any) map[string]any {
	c := map[string]any{"iss": p.srv.URL, "sub": "u-1", "aud": "client-1", "exp": t0.Add(time.Hour).Unix(),
		"email": "ada@example.com", "name": "Ada", "groups": []string{"eng", "ops"}}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func isKind(err error, k domain.ErrorKind) bool { return domain.KindOf(err) == k }

// RA-US-2.1: a valid token from an enabled provider is accepted.
func TestVerifyAccepts(t *testing.T) {
	p, v, _, prov := setup(t)
	id, err := v.Verify(t.Context(), p.sign(t, "k1", good(p, nil)), []auth.Provider{prov})
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u-1" || id.Email != "ada@example.com" || id.Name != "Ada" || id.Provider.ID != "idp_1" ||
		len(id.Groups) != 2 || id.Groups[0] != "eng" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestVerifyAudienceArrayAndES256(t *testing.T) {
	p, v, _, prov := setup(t)
	p.addEC(t, "ec1")
	tok := p.sign(t, "ec1", good(p, map[string]any{"aud": []string{"other", "client-1"}, "groups": "solo"}))
	id, err := v.Verify(t.Context(), tok, []auth.Provider{prov})
	if err != nil || len(id.Groups) != 1 || id.Groups[0] != "solo" {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	p, v, ck, prov := setup(t)
	other := newIDP(t)
	other.addRSA(t, "k1")
	cases := []struct {
		name  string
		token func() string
		provs []auth.Provider
		want  domain.ErrorKind
	}{
		{"disabled provider (not in the enabled list)", func() string { return p.sign(t, "k1", good(p, nil)) }, nil, domain.KindUnauthenticated},
		{"wrong audience", func() string { return p.sign(t, "k1", good(p, map[string]any{"aud": "someone-else"})) }, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"unknown issuer", func() string { return p.sign(t, "k1", good(p, map[string]any{"iss": "https://elsewhere.example"})) }, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"expired beyond leeway", func() string {
			return p.sign(t, "k1", good(p, map[string]any{"exp": t0.Add(-31 * time.Second).Unix()}))
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"no expiry", func() string {
			c := good(p, nil)
			delete(c, "exp")
			return p.sign(t, "k1", c)
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"not yet valid", func() string { return p.sign(t, "k1", good(p, map[string]any{"nbf": t0.Add(time.Hour).Unix()})) }, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"no subject", func() string { return p.sign(t, "k1", good(p, map[string]any{"sub": ""})) }, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"no email", func() string {
			c := good(p, nil)
			delete(c, "email")
			return p.sign(t, "k1", c)
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"signed by another provider's key", func() string {
			c := good(p, nil)
			return other.sign(t, "k1", c) // same kid, different key
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"garbage", func() string { return "not.a.jwt" }, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"unsigned (alg none)", func() string {
			// header {"alg":"none"} . payload . empty signature
			return "eyJhbGciOiJub25lIn0." + b64(good(p, nil)) + "."
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
		{"tampered payload", func() string {
			parts := splitJWT(p.sign(t, "k1", good(p, nil)))
			return parts[0] + "." + b64(good(p, map[string]any{"sub": "admin"})) + "." + parts[2]
		}, []auth.Provider{prov}, domain.KindUnauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := v.Verify(t.Context(), c.token(), c.provs); !isKind(err, c.want) {
				t.Fatalf("err = %v, want %s", err, c.want)
			}
		})
	}
	// Within the leeway an expired token is still accepted.
	ck.t = t0
	if _, err := v.Verify(t.Context(), p.sign(t, "k1", good(p, map[string]any{"exp": t0.Add(-29 * time.Second).Unix()})), []auth.Provider{prov}); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
}

func TestHMACTokenSignedWithPublicKeyIsRefused(t *testing.T) {
	p, v, _, prov := setup(t)
	p.mu.Lock()
	pk := p.keys["k1"].priv.(*rsa.PrivateKey).PublicKey
	p.mu.Unlock()
	secret := []byte(pk.N.String())
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(good(p, nil))
	obj, _ := s.Sign(payload)
	tok, _ := obj.CompactSerialize()
	if _, err := v.Verify(t.Context(), tok, []auth.Provider{prov}); !isKind(err, domain.KindUnauthenticated) {
		t.Fatalf("HS256 token accepted: %v", err)
	}
}

// RA-US-2.3: keys are cached, an unknown kid refetches once, a removed key stops validating.
func TestJWKSCacheAndRotation(t *testing.T) {
	p, v, ck, prov := setup(t)
	provs := []auth.Provider{prov}
	tok1 := p.sign(t, "k1", good(p, nil))
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(t.Context(), tok1, provs); err != nil {
			t.Fatal(err)
		}
	}
	if n := p.jwksHits.Load(); n != 1 {
		t.Fatalf("5 verifications fetched the JWKS %d times, want 1 (cached)", n)
	}

	// A new key: a token with an unknown kid triggers one refetch and is accepted.
	p.addRSA(t, "k2")
	ck.t = t0.Add(10 * time.Second) // past the minimum refetch interval, well inside the TTL
	tok2 := p.sign(t, "k2", good(p, map[string]any{"exp": ck.t.Add(time.Hour).Unix()}))
	if _, err := v.Verify(t.Context(), tok2, provs); err != nil {
		t.Fatalf("new key: %v", err)
	}
	if n := p.jwksHits.Load(); n != 2 {
		t.Fatalf("unknown kid fetched %d times, want exactly one refetch", n)
	}

	// An unknown kid again within the minimum interval does not refetch (no amplification).
	p.remove("k2")
	p.addRSA(t, "k3")
	tok3 := p.sign(t, "k3", good(p, map[string]any{"exp": ck.t.Add(time.Hour).Unix()}))
	if _, err := v.Verify(t.Context(), tok3, provs); !isKind(err, domain.KindUnauthenticated) {
		t.Fatalf("refetch inside the minimum interval: %v", err)
	}
	if n := p.jwksHits.Load(); n != 2 {
		t.Fatalf("fetched %d times inside the minimum interval", n)
	}

	// After the TTL the set is refetched: k2 is gone and stops validating; k3 now works.
	ck.t = t0.Add(DefaultTTL + time.Minute)
	if _, err := v.Verify(t.Context(), p.sign(t, "k3", good(p, map[string]any{"exp": ck.t.Add(time.Hour).Unix()})), provs); err != nil {
		t.Fatalf("k3 after TTL: %v", err)
	}
	if _, err := v.Verify(t.Context(), p.sign3(t, "k2", ck), provs); err == nil {
		t.Fatal("removed key still validates after the refetch")
	}
}

// sign3 signs with the (removed) k2 key kept only in the test: re-add it just to sign, then remove.
func (p *idp) sign3(t *testing.T, kid string, ck *clock) string {
	t.Helper()
	p.addRSA(t, kid)
	tok := p.sign(t, kid, good(p, map[string]any{"exp": ck.t.Add(time.Hour).Unix()}))
	p.remove(kid)
	return tok
}

// F-7: a JWKS fetch failure keeps using cached keys; with none cached it is 503, not 401.
func TestJWKSFailure(t *testing.T) {
	p, v, ck, prov := setup(t)
	provs := []auth.Provider{prov}
	tok := p.sign(t, "k1", good(p, nil))

	p.setDown(true)
	if _, err := v.Verify(t.Context(), tok, provs); !isKind(err, domain.KindUnavailable) {
		t.Fatalf("no cache, provider down: %v, want Unavailable", err)
	}
	// Inside the minimum interval the failure is remembered without another fetch.
	hits := p.jwksHits.Load()
	if _, err := v.Verify(t.Context(), tok, provs); !isKind(err, domain.KindUnavailable) {
		t.Fatalf("second try: %v", err)
	}
	if p.jwksHits.Load() != hits {
		t.Fatal("a failed fetch was retried inside the minimum interval")
	}

	p.setDown(false)
	ck.t = t0.Add(time.Minute)
	if _, err := v.Verify(t.Context(), tok, provs); err != nil {
		t.Fatalf("provider back: %v", err)
	}

	// Cached keys keep validating past the TTL while the provider is down.
	p.setDown(true)
	ck.t = t0.Add(DefaultTTL + time.Hour)
	tok = p.sign(t, "k1", good(p, map[string]any{"exp": ck.t.Add(time.Hour).Unix()}))
	if _, err := v.Verify(t.Context(), tok, provs); err != nil {
		t.Fatalf("stale cache while down: %v", err)
	}
}

func TestVerifyContextCancelled(t *testing.T) {
	p, v, _, prov := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := v.Verify(ctx, p.sign(t, "k1", good(p, nil)), []auth.Provider{prov}); !isKind(err, domain.KindUnavailable) {
		t.Fatalf("cancelled: %v", err)
	}
}

func b64(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return base64URL(raw)
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func splitJWT(s string) []string { return strings.Split(s, ".") }
