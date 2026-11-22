// Package tokenreview validates the optional X-Afs-Gateway-Token (gateway contract §3, M3-A3): a
// projected ServiceAccount token with audience afs-gateway, reviewed through the Kubernetes TokenReview
// API. A good result is cached for at most five minutes. When the API cannot be reached the review is
// unavailable, and the gateway fails closed.
package tokenreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// Audience is the audience of the gateway token (render.GatewayTokenAudience).
const Audience = "afs-gateway"

// MaxCacheAge bounds how long a good review is reused (GW-US-6.5).
const MaxCacheAge = 5 * time.Minute

const (
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"
	maxEntries   = 20_000
)

// Reviewer implements gateway.TokenReviewer.
type Reviewer struct {
	client kubernetes.Interface
	now    func() time.Time
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	id  gateway.TokenIdentity
	exp time.Time
}

// New returns a Reviewer. now is time.Now when nil.
func New(client kubernetes.Interface, now func() time.Time) *Reviewer {
	if now == nil {
		now = time.Now
	}
	return &Reviewer{client: client, now: now, ttl: MaxCacheAge, cache: map[string]cached{}}
}

var _ gateway.TokenReviewer = (*Reviewer)(nil)

// Review implements gateway.TokenReviewer.
func (r *Reviewer) Review(ctx context.Context, token string) (gateway.TokenIdentity, error) {
	sum := sha256.Sum256([]byte(token)) // the token itself is never kept as a key
	key := hex.EncodeToString(sum[:])
	if id, ok := r.lookup(key); ok {
		return id, nil
	}
	res, err := r.client.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return gateway.TokenIdentity{}, gateway.ErrReviewUnavailable
	}
	st := res.Status
	if !st.Authenticated || !hasAudience(st.Audiences, Audience) {
		return gateway.TokenIdentity{}, gateway.ErrTokenInvalid
	}
	id, ok := identity(st.User)
	if !ok {
		return gateway.TokenIdentity{}, gateway.ErrTokenInvalid // a token not bound to a pod names no pod
	}
	r.store(key, id)
	return id, nil
}

func hasAudience(got []string, want string) bool {
	for _, a := range got {
		if a == want {
			return true
		}
	}
	return false
}

func identity(u authv1.UserInfo) (gateway.TokenIdentity, bool) {
	// system:serviceaccount:<namespace>:<name>
	parts := strings.Split(u.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return gateway.TokenIdentity{}, false
	}
	first := func(k string) string {
		if v := u.Extra[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	name := first(extraPodName)
	if name == "" {
		return gateway.TokenIdentity{}, false
	}
	return gateway.TokenIdentity{Namespace: parts[2], PodName: name, PodUID: first(extraPodUID)}, true
}

func (r *Reviewer) lookup(key string) (gateway.TokenIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cache[key]
	if !ok {
		return gateway.TokenIdentity{}, false
	}
	if !r.now().Before(c.exp) {
		delete(r.cache, key)
		return gateway.TokenIdentity{}, false
	}
	return c.id, true
}

func (r *Reviewer) store(key string, id gateway.TokenIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxEntries {
		now := r.now()
		for k, c := range r.cache {
			if !now.Before(c.exp) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= maxEntries {
			clear(r.cache)
		}
	}
	r.cache[key] = cached{id: id, exp: r.now().Add(r.ttl)}
}
