package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/oidc"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// workloadSettings reads AFS_CLUSTER_OIDC_ISSUER: the cluster's service account issuer, an https URL
// (for EKS, https://oidc.eks.<region>.amazonaws.com/id/<cluster id>). afs-api only fetches its public
// keys; it never calls the Kubernetes API (RA-2b).
func workloadSettings(env func(string) string, s *settings) error {
	v := strings.TrimSpace(env("AFS_CLUSTER_OIDC_ISSUER"))
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("AFS_CLUSTER_OIDC_ISSUER: %q is not an https issuer URL", v)
	}
	s.ClusterOIDCIssuer = v
	return nil
}

// wireWorkload accepts projected ServiceAccount tokens (RA-2b, M6-D): offline validation against the
// cluster issuer's JWKS, then namespace and ServiceAccount to the agent or pool through
// afs.resolve_workload. With no issuer configured, workload tokens are refused like any unknown token.
func wireWorkload(c *components, s settings) {
	if s.ClusterOIDCIssuer == "" || c.Auth == nil {
		return
	}
	c.Auth.Workload = &auth.WorkloadAuth{
		Verifier: oidc.NewClusterVerifier(s.ClusterOIDCIssuer, oidc.Options{HTTP: s.OIDCHTTP}),
		Lookup:   c.Creds,
	}
}
