package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

func TestProviderRoles(t *testing.T) {
	const arn = "arn:aws:iam::111122223333:role/afs-viewers"
	good := map[string]struct {
		raw  string
		want map[string]string
	}{
		"unset":   {"", nil},
		"blank":   {"  \n", nil},
		"default": {`{"default":"` + arn + `"}`, map[string]string{"default": arn}},
		"by id":   {`{"idp_01JD0000000000000000000001":"` + arn + `"}`, map[string]string{"idp_01JD0000000000000000000001": arn}},
		"path":    {`{"default":"arn:aws-us-gov:iam::111122223333:role/team/viewers"}`, map[string]string{"default": "arn:aws-us-gov:iam::111122223333:role/team/viewers"}},
	}
	for name, tc := range good {
		t.Run(name, func(t *testing.T) {
			got, err := providerRoles(tc.raw)
			if err != nil || len(got) != len(tc.want) {
				t.Fatalf("got %v, %v", got, err)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
	bad := map[string]string{
		"not json":      `default=` + arn,
		"not strings":   `{"default":1}`,
		"unknown key":   `{"okta":"` + arn + `"}`,
		"not a role":    `{"default":"arn:aws:iam::111122223333:user/alice"}`,
		"empty":         `{"default":""}`,
		"other service": `{"default":"arn:aws:s3:::bucket"}`,
		"short account": `{"default":"arn:aws:iam::1111:role/x"}`,
	}
	for name, raw := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := providerRoles(raw); err == nil || !strings.Contains(err.Error(), "AFS_VIEWER_KUBECTL_ROLES") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// M5-E2: the kubectl routes share the teams table with the other controllers, and a service-level identity
// provider change reconciles every team's viewer access.
func TestRoutesCombineKubectlRoutes(t *testing.T) {
	reg, err := routes(append(append(links.Routes(), kubectl.Routes()...), tags.Routes()...)...)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	teamRef := engine.NewRef(engine.KindTeam, "team_A", "team_A")
	kcA := engine.NewRef(kubectl.Kind, "team_A", "team_A")
	kcB := engine.NewRef(kubectl.Kind, "team_B", "team_B")
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		if slices.Contains(kinds, kubectl.Kind) {
			return []engine.Ref{kcA, kcB}, nil
		}
		return nil, nil
	}
	got, err := reg.Resolve(t.Context(), engine.Change{Table: "teams", ID: "team_A", TeamID: "team_A"}, fan)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, teamRef) || !slices.Contains(got, kcA) {
		t.Errorf("teams change reached %v, want the team and its viewer access", got)
	}
	got, err = reg.Resolve(t.Context(), engine.Change{Table: "identity_providers", ID: "idp_1"}, fan)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, kcA) || !slices.Contains(got, kcB) {
		t.Errorf("a service-level provider change reached %v, want every team's viewer access", got)
	}
}
