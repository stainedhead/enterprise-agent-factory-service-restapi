// Package oidc validates identity provider access tokens for afs-api (RA-2). It fetches each
// provider's JWKS through OIDC discovery, caches it for ten minutes, refetches once when a token names
// an unknown key id, and keeps validating with cached keys while the provider is unreachable.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const (
	// DefaultTTL is how long a fetched key set is trusted (API contract §2, RA-US-2.3).
	DefaultTTL = 10 * time.Minute
	// DefaultMinRefetch bounds how often a key set is refetched, so a flood of tokens with random key
	// ids, or an outage, cannot turn into a flood of requests to the provider.
	DefaultMinRefetch = 5 * time.Second
	// DefaultLeeway is the clock skew tolerated on exp and nbf.
	DefaultLeeway = 30 * time.Second

	maxDocument = 1 << 20
)

// Options configure a Verifier. The zero value is usable.
type Options struct {
	// HTTP fetches discovery documents and key sets; default: a client with a 5 s timeout.
	HTTP *http.Client
	// Now returns the current time; default time.Now.
	Now func() time.Time
	// TTL, MinRefetch and Leeway default to the package constants.
	TTL, MinRefetch, Leeway time.Duration
}

func (o *Options) defaults() {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.MinRefetch <= 0 {
		o.MinRefetch = DefaultMinRefetch
	}
	if o.Leeway <= 0 {
		o.Leeway = DefaultLeeway
	}
}

// keySet caches the keys of one issuer.
type keySet struct {
	issuer string

	mu          sync.Mutex // held while fetching: callers that wait find the keys already refreshed
	jwksURI     string
	keys        []jose.JSONWebKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

func (v *Verifier) keySet(issuer string) *keySet {
	v.mu.Lock()
	defer v.mu.Unlock()
	ks := v.sets[issuer]
	if ks == nil {
		ks = &keySet{issuer: issuer}
		v.sets[issuer] = ks
	}
	return ks
}

// key returns the key kid of issuer, refetching the set when it is older than the TTL or when kid is
// unknown (at most once per MinRefetch).
func (v *Verifier) key(ctx context.Context, issuer, kid string) (jose.JSONWebKey, error) {
	ks := v.keySet(issuer)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	now := v.opts.Now()
	fresh := !ks.fetchedAt.IsZero() && now.Sub(ks.fetchedAt) < v.opts.TTL
	k, found := ks.find(kid)
	if !fresh || !found {
		never := ks.lastAttempt.IsZero()
		if never || now.Sub(ks.lastAttempt) >= v.opts.MinRefetch {
			ks.lastAttempt = now
			if err := v.fetch(ctx, ks); err != nil {
				if len(ks.keys) == 0 {
					return jose.JSONWebKey{}, domain.Unavailable("identity provider keys", err)
				}
				// Keep validating with the cached keys while the provider is down.
			} else {
				ks.fetchedAt = now
			}
			k, found = ks.find(kid)
		} else if len(ks.keys) == 0 {
			return jose.JSONWebKey{}, domain.Unavailable("identity provider keys", errors.New("fetch failed recently"))
		}
	}
	if !found {
		return jose.JSONWebKey{}, domain.Unauthenticated()
	}
	return k, nil
}

// find returns the key named kid; a token without kid matches a set of exactly one key.
func (ks *keySet) find(kid string) (jose.JSONWebKey, bool) {
	if kid == "" {
		if len(ks.keys) == 1 {
			return ks.keys[0], true
		}
		return jose.JSONWebKey{}, false
	}
	for _, k := range ks.keys {
		if k.KeyID == kid {
			return k, true
		}
	}
	return jose.JSONWebKey{}, false
}

func (v *Verifier) fetch(ctx context.Context, ks *keySet) error {
	if ks.jwksURI == "" {
		var doc struct {
			JWKSURI string `json:"jwks_uri"`
		}
		disc := strings.TrimRight(ks.issuer, "/") + "/.well-known/openid-configuration"
		if err := v.getJSON(ctx, disc, &doc); err != nil {
			return fmt.Errorf("discovery: %w", err)
		}
		u, err := url.Parse(doc.JWKSURI)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("discovery: jwks_uri is not an https URL")
		}
		ks.jwksURI = doc.JWKSURI
	}
	var set jose.JSONWebKeySet
	if err := v.getJSON(ctx, ks.jwksURI, &set); err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	keys := make([]jose.JSONWebKey, 0, len(set.Keys))
	for _, k := range set.Keys {
		// Skip private material and keys that are not for signature verification.
		if k.IsPublic() && k.Valid() && (k.Use == "" || k.Use == "sig") {
			keys = append(keys, k)
		}
	}
	ks.keys = keys
	return nil
}

func (v *Verifier) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.opts.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(dst)
}
