package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// TestAuthorizationMatrix runs the oracle-based matrix over every implemented operation.
func TestAuthorizationMatrix(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := wire(context.Background(), quiet, withCluster(settings{
		DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6, AWSRegion: "us-east-1", ClusterOIDCIssuer: w.Cluster.Server.URL,
		McpOAuth: testMcpOAuth(w),
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	// M6-F1 (CT-API-7): no placeholder remains; every operation of the contract has a bound handler.
	contract, err := matrix.LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, id := range matrix.Implemented(t, w, c.Handler, contract) {
		have[id] = true
	}
	for id := range contract.Operations {
		if !have[id] && !pendingV150[id] {
			t.Errorf("operation %s still answers the 501 not_implemented placeholder", id)
		}
	}
	for id := range pendingV150 {
		if have[id] {
			t.Errorf("operation %s is implemented: remove it from pendingV150", id)
		}
	}
	matrix.Run(t, w, c.Handler)
}

// pendingV150 is httpapi.PendingModelEndpointOps and httpapi.PendingMcpOps: the v1.5.0 §4.13 and v1.6.0 §4.14
// operations whose handlers land in the implementation changes of releases 1.2.0 and 1.3.0
// (specs/261125-model-endpoint-gateway, specs/261125-mcp-gateway). The lists may only shrink.
var pendingV150 = func() map[string]bool {
	m := map[string]bool{}
	for _, op := range httpapi.PendingModelEndpointOps {
		m[op] = true
	}
	for _, op := range httpapi.PendingMcpOps {
		m[op] = true
	}
	return m
}()
