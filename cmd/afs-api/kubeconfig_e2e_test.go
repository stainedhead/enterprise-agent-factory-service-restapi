package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// withCluster gives afs-api the cluster GetTeamKubeconfig renders (OQ-S5): a dummy endpoint and CA.
func withCluster(s settings) settings {
	s.ClusterName = "afs"
	s.ClusterEndpoint = "https://EXAMPLE0123456789.gr7.us-east-1.eks.amazonaws.com"
	s.ClusterCAData = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCkVYQU1QTEUKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="
	return s
}

// RA-US-12.1 through the real pipeline and database: with viewerKubectl off a viewer and a team-admin get
// 403 and one denied audit record; with it on they get a kubeconfig that uses the exec credential plugin
// and holds no token. A cluster that is not configured is 503. The authorization matrix runs the on case
// for every caller (fixtures_kubeconfig.go); this test is its off half.
func TestKubeconfigFlagOffIsRefused(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, withCluster(settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6, AWSRegion: "us-east-1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	path := "/v1/teams/" + w.TeamA.ID + "/kubeconfig"
	denied := func() int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'GetTeamKubeconfig' AND outcome = 'denied' AND team_id = $1`, w.TeamA.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	w.SetFlags(t, authz.TeamFlags{})
	for _, caller := range []matrix.Caller{matrix.Viewer, matrix.TeamAdmin, matrix.TeamKeyViewer} {
		before := denied()
		code, out, _ := ldCall(t, c.Handler, http.MethodGet, path, w.Tokens[caller], "")
		if code != 403 || !strings.Contains(out, "teams:get-kubeconfig") {
			t.Errorf("%s with the flag off: %d %s", caller, code, out)
		}
		if got := denied(); got != before+1 {
			t.Errorf("%s: denied audit records %d -> %d, want one more", caller, before, got)
		}
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, path, w.Tokens[matrix.ServiceAdmin], ""); code != 200 {
		t.Errorf("a service admin with the flag off: %d, want 200", code)
	}

	w.SetFlags(t, authz.TeamFlags{ViewerKubectl: true})
	code, out, hdr := ldCall(t, c.Handler, http.MethodGet, path, w.Tokens[matrix.Viewer], "")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/yaml") {
		t.Fatalf("viewer with the flag on: %d %s %v", code, out, hdr)
	}
	for _, want := range []string{"command: aws", "get-token", "--cluster-name", "namespace: afs-t-"} {
		if !strings.Contains(out, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"token:", "client-key", "Bearer"} {
		if strings.Contains(out, bad) {
			t.Errorf("kubeconfig contains %q:\n%s", bad, out)
		}
	}

	// A team that does not exist is a 404 for a service admin, never a document for it.
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/teams/team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ/kubeconfig", w.Tokens[matrix.ServiceAdmin], ""); code != 404 {
		t.Errorf("unknown team: %d, want 404", code)
	}
}

func TestKubeconfigWithoutAClusterIsUnavailable(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w.SetFlags(t, authz.TeamFlags{ViewerKubectl: true})
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/teams/"+w.TeamA.ID+"/kubeconfig", w.Tokens[matrix.Viewer], ""); code != 503 {
		t.Errorf("no cluster endpoint: %d, want 503", code)
	}
}
