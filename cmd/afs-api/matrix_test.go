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
	setClusterEnv(t) // GetTeamKubeconfig renders the cluster from the environment (M5-E1)
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := wire(context.Background(), quiet, settings{
		DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6, AWSRegion: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	matrix.Run(t, w, c.Handler)
}
