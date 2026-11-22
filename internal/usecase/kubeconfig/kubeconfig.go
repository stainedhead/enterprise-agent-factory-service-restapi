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
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

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

// Render builds the kubeconfig document of a team: one cluster, one context named for the namespace and
// one user whose credentials come from the exec plugin. There is no token, certificate or key in it.
func Render(c Cluster, t Team) ([]byte, error) {
	ctxName := "afs-" + t.Slug
	doc := document{
		APIVersion: "v1", Kind: "Config", CurrentContext: ctxName,
		Clusters: []namedCluster{{Name: "afs", Cluster: clusterBody{Server: c.Endpoint, CAData: c.CAData}}},
		Contexts: []namedContext{{Name: ctxName, Context: contextBody{Cluster: "afs", Namespace: t.Namespace, User: "afs"}}},
		Users: []namedUser{{Name: "afs", User: userBody{Exec: execBody{
			APIVersion: "client.authentication.k8s.io/v1beta1", Command: "aws",
			Args:               []string{"eks", "get-token", "--cluster-name", c.Name, "--region", c.Region},
			InteractiveMode:    "Never",
			ProvideClusterInfo: false,
		}}}},
	}
	return yaml.Marshal(doc)
}

type document struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	Clusters       []namedCluster `yaml:"clusters"`
	Contexts       []namedContext `yaml:"contexts"`
	CurrentContext string         `yaml:"current-context"`
	Users          []namedUser    `yaml:"users"`
}

type namedCluster struct {
	Name    string      `yaml:"name"`
	Cluster clusterBody `yaml:"cluster"`
}

type clusterBody struct {
	Server string `yaml:"server"`
	CAData string `yaml:"certificate-authority-data"`
}

type namedContext struct {
	Name    string      `yaml:"name"`
	Context contextBody `yaml:"context"`
}

type contextBody struct {
	Cluster   string `yaml:"cluster"`
	Namespace string `yaml:"namespace"`
	User      string `yaml:"user"`
}

type namedUser struct {
	Name string   `yaml:"name"`
	User userBody `yaml:"user"`
}

type userBody struct {
	Exec execBody `yaml:"exec"`
}

type execBody struct {
	APIVersion         string   `yaml:"apiVersion"`
	Command            string   `yaml:"command"`
	Args               []string `yaml:"args"`
	InteractiveMode    string   `yaml:"interactiveMode"`
	ProvideClusterInfo bool     `yaml:"provideClusterInfo"`
}
