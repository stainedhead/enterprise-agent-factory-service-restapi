// Package routeauth supplies the upstream credential of a model route (gateway contract §12.2, PRD §11.1):
// bearer and header routes read the credential the reconciler mounts from Secrets Manager into the team
// gateway (one file per route, re-read when it changes, so rotation needs no restart); federated routes
// exchange the gateway's projected ServiceAccount token for an Azure Entra or a Google access token.
// Values stay in memory; nothing here logs or returns them beyond the call that sends them.
package routeauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// Federation is a team's federation record of a federated endpoint (afs.endpoint_federations).
type Federation struct {
	Kind          string // azure or google
	TenantID      string
	ClientID      string
	ProjectNumber string
	PoolID        string
	ProviderID    string
	ProjectID     string
	Location      string
	Status        string // pending, ready or failing
}

// FederationStore reads federation records (afs_gateway).
type FederationStore interface {
	// Federation returns the team's record for the endpoint; gateway.ErrNotFound when none is set.
	Federation(ctx context.Context, endpointID, teamID string) (Federation, error)
	// ReportFederation records the outcome of an exchange (status ready or failing, a short error).
	ReportFederation(ctx context.Context, endpointID, teamID, status, lastError string) error
}

// Exchanger turns the gateway's identity into an access token for a federation.
type Exchanger interface {
	Exchange(ctx context.Context, f Federation) (token string, expires time.Time, err error)
}

// Auth implements gateway.UpstreamAuth.
type Auth struct {
	// Dir holds one file per route slug (and <slug>.<teamId> is not used: a team gateway mounts only its
	// own team's credentials, the shared one or the team's own, under the slug).
	Dir         string
	Federations FederationStore
	Azure       Exchanger
	Google      Exchanger
	Now         func() time.Time

	mu     sync.Mutex
	files  map[string]fileEntry
	tokens map[string]tokenEntry
}

type fileEntry struct {
	mod   time.Time
	size  int64
	value string
}

type tokenEntry struct {
	token   string
	expires time.Time
}

var _ gateway.UpstreamAuth = (*Auth)(nil)

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Credential implements gateway.UpstreamAuth.
func (a *Auth) Credential(ctx context.Context, ep gateway.RouteEndpoint, teamID string) (gateway.UpstreamCredential, error) {
	switch ep.AuthKind {
	case domain.AuthNone, "":
		return gateway.UpstreamCredential{}, nil
	case domain.AuthBearer, domain.AuthHeader:
		v, err := a.file(ep.Slug)
		if err != nil {
			return gateway.UpstreamCredential{}, err
		}
		if ep.AuthKind == domain.AuthBearer {
			return gateway.UpstreamCredential{Header: "Authorization", Value: "Bearer " + v}, nil
		}
		return gateway.UpstreamCredential{Header: ep.AuthHeader, Value: v}, nil
	case domain.AuthAzureFederated, domain.AuthGoogleFederated:
		tok, err := a.federated(ctx, ep, teamID)
		if err != nil {
			return gateway.UpstreamCredential{}, err
		}
		return gateway.UpstreamCredential{Header: "Authorization", Value: "Bearer " + tok}, nil
	}
	return gateway.UpstreamCredential{}, gateway.ErrCredentialMissing
}

// file reads the mounted credential of a route, again whenever the file changes.
func (a *Auth) file(slug string) (string, error) {
	if a.Dir == "" || domain.ValidRouteSlug(slug) != nil {
		return "", gateway.ErrCredentialMissing
	}
	p := filepath.Join(a.Dir, slug)
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", gateway.ErrCredentialMissing
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.files == nil {
		a.files = map[string]fileEntry{}
	}
	if e, ok := a.files[slug]; ok && e.mod.Equal(st.ModTime()) && e.size == st.Size() {
		return e.value, nil
	}
	b, err := os.ReadFile(p) //nolint:gosec // the path is the mount directory and a validated slug
	if err != nil {
		return "", gateway.ErrCredentialMissing
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", gateway.ErrCredentialMissing
	}
	a.files[slug] = fileEntry{mod: st.ModTime(), size: st.Size(), value: v}
	return v, nil
}

// refreshBefore is how long before expiry a token is exchanged again.
const refreshBefore = 5 * time.Minute

func (a *Auth) federated(ctx context.Context, ep gateway.RouteEndpoint, teamID string) (string, error) {
	key := ep.ID + "/" + teamID
	a.mu.Lock()
	if e, ok := a.tokens[key]; ok && a.now().Add(refreshBefore).Before(e.expires) {
		a.mu.Unlock()
		return e.token, nil
	}
	a.mu.Unlock()
	if a.Federations == nil {
		return "", gateway.ErrFederationNotReady
	}
	f, err := a.Federations.Federation(ctx, ep.ID, teamID)
	if err != nil {
		if errors.Is(err, gateway.ErrNotFound) {
			return "", gateway.ErrFederationNotReady
		}
		return "", err
	}
	ex := a.Azure
	if ep.AuthKind == domain.AuthGoogleFederated {
		ex = a.Google
	}
	if ex == nil || f.Kind != ep.AuthKind.FederationKind() {
		return "", gateway.ErrFederationNotReady
	}
	tok, exp, err := ex.Exchange(ctx, f)
	if err != nil {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if f.Status != "failing" {
			_ = a.Federations.ReportFederation(context.WithoutCancel(ctx), ep.ID, teamID, "failing", msg)
		}
		return "", errors.Join(gateway.ErrFederationNotReady, err)
	}
	if f.Status != "ready" {
		_ = a.Federations.ReportFederation(context.WithoutCancel(ctx), ep.ID, teamID, "ready", "")
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]tokenEntry{}
	}
	a.tokens[key] = tokenEntry{token: tok, expires: exp}
	a.mu.Unlock()
	return tok, nil
}
