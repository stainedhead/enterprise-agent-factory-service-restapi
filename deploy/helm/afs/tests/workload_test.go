package tests

import "testing"

// M6-D: the cluster's service account issuer reaches afs-api (and only afs-api) as an environment
// variable, so workload tokens validate against it; with none configured the variable is absent and the
// API refuses workload tokens.
func TestClusterOIDCIssuerReachesTheAPI(t *testing.T) {
	docs := render(t, "")
	if _, ok := deployEnv(t, docs, "afs-api")["AFS_CLUSTER_OIDC_ISSUER"]; ok {
		t.Error("AFS_CLUSTER_OIDC_ISSUER rendered with no issuer configured")
	}
	docs = render(t, `global:
  cluster:
    oidcIssuer: https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE
`)
	if got := deployEnv(t, docs, "afs-api")["AFS_CLUSTER_OIDC_ISSUER"]; got != "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLED539D4633E53DE1B71EXAMPLE" {
		t.Errorf("api AFS_CLUSTER_OIDC_ISSUER = %q", got)
	}
	for _, name := range []string{"afs-reconciler", "afs-gateway"} {
		if _, ok := deployEnv(t, docs, name)["AFS_CLUSTER_OIDC_ISSUER"]; ok {
			t.Errorf("%s gets AFS_CLUSTER_OIDC_ISSUER", name)
		}
	}
}
