package main

import (
	crand "crypto/rand"
	"fmt"
	"net/url"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
)

// gatewaySettings reads AFS_SHARED_GATEWAY_URL, the shared afs-gateway's URL (https or http inside the
// cluster), shown to teams on the shared gateway.
func gatewaySettings(env func(string) string, s *settings) error {
	v := strings.TrimSpace(env("AFS_SHARED_GATEWAY_URL"))
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("AFS_SHARED_GATEWAY_URL: %q is not a gateway URL", v)
	}
	s.SharedGatewayURL = strings.TrimRight(v, "/")
	return nil
}

// wireModelEndpoints builds the model endpoint use cases (API contract v1.5.0 §4.13) and binds their
// handlers. The federation hints (issuer, account, partition) come from the platform settings; credentials
// share the Secrets Manager writer of wireSecrets.
func wireModelEndpoints(c *components, s settings) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	partition := ""
	if s.AWSRegion != "" {
		partition = aws.PartitionOf(s.AWSRegion)
	}
	svc := &modelendpoints.Service{Deps: deps, Repo: postgres.NewModelEndpointsRepo(), Secrets: c.SecretsWriter,
		Config: modelendpoints.Config{OIDCIssuer: s.ClusterOIDCIssuer, Partition: partition, AccountID: s.AWSAccountID, SharedGatewayURL: s.SharedGatewayURL}}
	c.bind(httpapi.BindModelEndpoints(&httpapi.ModelEndpointsAPI{Svc: svc, Pages: httpapi.NewPageTokens(c.PageKey, nil)}))
}
