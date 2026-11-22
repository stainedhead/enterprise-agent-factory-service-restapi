package oidc

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// MaxWorkloadTokenLifetime is the longest a projected ServiceAccount token may live (RA-2b). The
// Kubernetes API server may issue tokens that last a year when extended expiration is on; a token that
// long is a stolen-token risk and is refused.
const MaxWorkloadTokenLifetime = time.Hour

// workerPodPattern is afs.worker_pod_ok in the database.
var workerPodPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,251}[a-z0-9])?$`)

// ClusterVerifier implements auth.ClusterVerifier: it validates the projected ServiceAccount tokens of
// agents and pools against the keys of the cluster's OIDC issuer (an EKS issuer URL). The keys come
// from the issuer's discovery document and JWKS and are cached like the identity provider keys, so a
// token validates with the Kubernetes API unreachable and no Kubernetes client exists in afs-api.
type ClusterVerifier struct {
	issuer string
	v      *Verifier
}

var _ auth.ClusterVerifier = (*ClusterVerifier)(nil)

// NewClusterVerifier returns a verifier for tokens issued by issuer.
func NewClusterVerifier(issuer string, opts Options) *ClusterVerifier {
	return &ClusterVerifier{issuer: normalize(issuer), v: NewVerifier(opts)}
}

// Issuer implements auth.ClusterVerifier.
func (c *ClusterVerifier) Issuer() string { return c.issuer }

type clusterClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    *float64        `json:"exp"`
	NotBefore *float64        `json:"nbf"`
	IssuedAt  *float64        `json:"iat"`
	K8s       struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
		Pod struct {
			Name string `json:"name"`
		} `json:"pod"`
	} `json:"kubernetes.io"`
}

// Verify implements auth.ClusterVerifier (RA-US-2b.3, RA-US-2b.4).
func (c *ClusterVerifier) Verify(ctx context.Context, token string) (auth.ClusterToken, error) {
	jws, err := jose.ParseSignedCompact(token, signatureAlgorithms)
	if err != nil || len(jws.Signatures) != 1 {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	// The payload is read before the signature is checked only to refuse another issuer without
	// fetching its keys; nothing in it is trusted until the signature verifies.
	var claimed clusterClaims
	if json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &claimed) != nil || normalize(claimed.Issuer) != c.issuer {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	k, err := c.v.key(ctx, c.issuer, jws.Signatures[0].Header.KeyID)
	if err != nil {
		return auth.ClusterToken{}, err
	}
	payload, err := jws.Verify(k)
	if err != nil {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	var cl clusterClaims
	if json.Unmarshal(payload, &cl) != nil || normalize(cl.Issuer) != c.issuer {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	// Exactly one audience: a token that also names afs-gateway (or the API server) could be replayed
	// elsewhere.
	if aud := audiences(cl.Audience); !slices.Equal(aud, []string{auth.WorkloadAudience}) {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	if err := c.checkTimes(cl); err != nil {
		return auth.ClusterToken{}, err
	}
	ns, sa, err := workloadSubject(cl)
	if err != nil {
		return auth.ClusterToken{}, err
	}
	pod := cl.K8s.Pod.Name
	if !workerPodPattern.MatchString(pod) {
		pod = "" // never let an odd name reach the audit row's check constraint
	}
	return auth.ClusterToken{Namespace: ns, ServiceAccount: sa, Pod: pod}, nil
}

func (c *ClusterVerifier) checkTimes(cl clusterClaims) error {
	opts := c.v.opts
	now := opts.Now()
	if cl.Expiry == nil || !now.Before(unix(*cl.Expiry).Add(opts.Leeway)) {
		return domain.Unauthenticated()
	}
	if cl.NotBefore != nil && now.Add(opts.Leeway).Before(unix(*cl.NotBefore)) {
		return domain.Unauthenticated()
	}
	// Expiry at most an hour out, and a token that was issued for longer than an hour is refused
	// even if it is about to expire.
	exp := unix(*cl.Expiry)
	if exp.Sub(now) > MaxWorkloadTokenLifetime+opts.Leeway {
		return domain.Unauthenticated()
	}
	if cl.IssuedAt != nil && exp.Sub(unix(*cl.IssuedAt)) > MaxWorkloadTokenLifetime+opts.Leeway {
		return domain.Unauthenticated()
	}
	return nil
}

// workloadSubject reads the namespace and ServiceAccount from the subject
// (system:serviceaccount:<namespace>:<name>); a kubernetes.io claim, when present, must agree.
func workloadSubject(cl clusterClaims) (ns, sa string, err error) {
	parts := strings.Split(cl.Subject, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return "", "", domain.Unauthenticated()
	}
	ns, sa = parts[2], parts[3]
	if (cl.K8s.Namespace != "" && cl.K8s.Namespace != ns) || (cl.K8s.ServiceAccount.Name != "" && cl.K8s.ServiceAccount.Name != sa) {
		return "", "", domain.Unauthenticated()
	}
	return ns, sa, nil
}
