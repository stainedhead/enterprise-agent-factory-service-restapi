package main

import (
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/kubeconfig"
)

// wireKubeconfig builds the kubeconfig use case (M5-E1) and binds its handler. The cluster comes from
// AFS_CLUSTER_NAME, AFS_CLUSTER_ENDPOINT and AFS_CLUSTER_CA_DATA (OQ-S5) and the region from AFS_AWS_REGION;
// afs-api never calls the cluster. With any of them unset, GetTeamKubeconfig answers 503.
func wireKubeconfig(c *components, s settings) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	httpapi.BindKubeconfig(&httpapi.KubeconfigAPI{Svc: &kubeconfig.Service{Deps: deps, Repo: postgres.NewKubeconfigRepo(),
		Cluster: kubeconfig.Cluster{Name: s.ClusterName, Endpoint: s.ClusterEndpoint,
			CAData: s.ClusterCAData, Region: s.AWSRegion}}})
}

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// platformSettings reads and validates AFS_AWS_ACCOUNT_ID and AFS_CLUSTER_NAME, AFS_CLUSTER_ENDPOINT and
// AFS_CLUSTER_CA_DATA, so a bad value stops startup instead of surfacing as a broken kubeconfig or role ARN.
func platformSettings(env func(string) string, s *settings) error {
	if v := env("AFS_AWS_ACCOUNT_ID"); v != "" {
		if !accountIDPattern.MatchString(v) {
			return fmt.Errorf("AFS_AWS_ACCOUNT_ID: %q is not a 12-digit account id", v)
		}
		s.AWSAccountID = v
	}
	s.ClusterName = env("AFS_CLUSTER_NAME")
	if v := env("AFS_CLUSTER_ENDPOINT"); v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("AFS_CLUSTER_ENDPOINT: %q is not an https URL", v)
		}
		s.ClusterEndpoint = v
	}
	if v := env("AFS_CLUSTER_CA_DATA"); v != "" {
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			return fmt.Errorf("AFS_CLUSTER_CA_DATA: not base64")
		}
		s.ClusterCAData = v
	}
	return nil
}
