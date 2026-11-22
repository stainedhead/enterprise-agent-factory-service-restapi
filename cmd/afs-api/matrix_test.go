package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

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
		if !have[id] {
			t.Errorf("operation %s still answers the 501 not_implemented placeholder", id)
		}
	}
	matrix.Run(t, w, c.Handler)
}
