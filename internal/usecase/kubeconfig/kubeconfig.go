// Package kubeconfig implements GetTeamKubeconfig (API contract §4.2, M5-E1, RA-10, ACC-5): the
// read-only kubeconfig of a team's namespace for viewers whose team has viewerKubectl on.
//
// The file names the cluster (endpoint and CA from AFS_CLUSTER_ENDPOINT and AFS_CLUSTER_CA_DATA, OQ-S5)
// and gets its credentials from the `aws eks get-token` exec plugin, so it holds no token: the caller's
// own AWS identity (the federated role) signs in when kubectl runs. Whether that identity may do anything
// is decided by the cluster (the access entry and the namespace Role the reconciler keeps, RC-13), never
// by this file. afs-api never calls Kubernetes or EKS; it only renders this document.
package kubeconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Cluster is the EKS cluster the kubeconfig points at.
type Cluster struct {
	// Name is the EKS cluster name (AFS_CLUSTER_NAME), the argument of `aws eks get-token`.
	Name string
	// Endpoint is the API server URL (AFS_CLUSTER_ENDPOINT), https only.
	Endpoint string
	// CAData is the base64 PEM certificate authority (AFS_CLUSTER_CA_DATA).
	CAData string
	// Region is the cluster's AWS region (AFS_AWS_REGION).
	Region string
}

// Validate reports what is missing or malformed in the configuration.
func (c Cluster) Validate() error {
	var missing []string
	for _, f := range []struct{ name, v string }{{"AFS_CLUSTER_NAME", c.Name}, {"AFS_CLUSTER_ENDPOINT", c.Endpoint},
		{"AFS_CLUSTER_CA_DATA", c.CAData}, {"AFS_AWS_REGION", c.Region}} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("kubeconfig: %s not set", strings.Join(missing, ", "))
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("kubeconfig: AFS_CLUSTER_ENDPOINT is not an https URL")
	}
	if _, err := base64.StdEncoding.DecodeString(c.CAData); err != nil {
		return errors.New("kubeconfig: AFS_CLUSTER_CA_DATA is not base64")
	}
	return nil
}

// Team is what the kubeconfig needs of a team.
type Team struct {
	Slug string
	// Namespace is the team's namespace (afs-t-<slug>).
	Namespace string
	// ViewerKubectl is the team's guardrail flag.
	ViewerKubectl bool
}

// Repository reads the team. Row-level security shows a caller only the teams it may see.
type Repository interface {
	// Team returns the team, or a domain NotFound error.
	Team(ctx context.Context, tx port.Tx, teamID string) (Team, error)
}

// Service is the kubeconfig use case.
type Service struct {
	acting.Deps
	Repo    Repository
	Cluster Cluster
}

// Get implements GetTeamKubeconfig: teams:get-kubeconfig is held by viewers and team-admins only while the
// team's viewerKubectl is on (403 with a denied audit record otherwise) and always by service admins. A
// cluster that is not configured answers 503 rather than a file that cannot work.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string) ([]byte, error) {
	var t Team
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		t, err = s.Repo.Team(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return nil, err
	}
	res := authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID, Flags: authz.TeamFlags{ViewerKubectl: t.ViewerKubectl}}
	switch authz.Authorize(c.Principal, authz.TeamsGetKubeconfig, res).Outcome {
	case authz.Allow:
	case authz.NotFound:
		return nil, domain.NotFound("team")
	default:
		return nil, s.Denied(ctx, c, "GetTeamKubeconfig", teamID, authz.TeamsGetKubeconfig)
	}
	if err := s.Cluster.Validate(); err != nil {
		return nil, domain.Unavailable("the cluster endpoint", err)
	}
	return Render(s.Cluster, t)
}

// plainScalar matches strings that are the same string as a plain YAML scalar: they start with a letter,
// hold no space, quote or "#", and do not end in a colon.
var plainScalar = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._/:=+-]*[A-Za-z0-9._/=+-]$|^[A-Za-z]$`)

// yamlWords are the plain scalars YAML reads as a boolean or null.
var yamlWords = map[string]bool{"true": true, "false": true, "null": true, "yes": true, "no": true, "on": true, "off": true, "y": true, "n": true}

// Render builds the kubeconfig document of a team: one cluster, one context named for the namespace and
// one user whose credentials come from the exec plugin. There is no token, certificate or key in it.
func Render(c Cluster, t Team) ([]byte, error) {
	q := func(v string) string {
		if plainScalar.MatchString(v) && !yamlWords[strings.ToLower(v)] {
			return v
		}
		b, _ := json.Marshal(v) // a JSON string is a valid YAML double-quoted scalar
		return string(b)
	}
	ctxName := "afs-" + t.Slug
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("apiVersion: v1\nkind: Config\ncurrent-context: %s\n", q(ctxName))
	w("clusters:\n  - name: afs\n    cluster:\n      server: %s\n      certificate-authority-data: %s\n", q(c.Endpoint), q(c.CAData))
	w("contexts:\n  - name: %s\n    context:\n      cluster: afs\n      namespace: %s\n      user: afs\n", q(ctxName), q(t.Namespace))
	w("users:\n  - name: afs\n    user:\n      exec:\n        apiVersion: client.authentication.k8s.io/v1beta1\n        command: aws\n")
	w("        args:\n          - eks\n          - get-token\n          - --cluster-name\n          - %s\n          - --region\n          - %s\n", q(c.Name), q(c.Region))
	w("        interactiveMode: Never\n        provideClusterInfo: false\n")
	return []byte(b.String()), nil
}
