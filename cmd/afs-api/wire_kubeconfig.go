package main

import (
	crand "crypto/rand"
	"os"

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
		Cluster: kubeconfig.Cluster{Name: os.Getenv("AFS_CLUSTER_NAME"), Endpoint: os.Getenv("AFS_CLUSTER_ENDPOINT"),
			CAData: os.Getenv("AFS_CLUSTER_CA_DATA"), Region: s.AWSRegion}}})
}
