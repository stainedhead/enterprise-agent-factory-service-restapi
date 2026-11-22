package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eks"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

// kubectlWiring is the wiring of viewer kubectl access (M5-E2, RC-13): the controller, its routes and a
// lister wrapper that adds one kubectl resource per live team.
type kubectlWiring struct {
	Controller *kubectl.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
}

// roleARNRE is an IAM role ARN, with or without a path.
var roleARNRE = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)

// providerRoles parses AFS_VIEWER_KUBECTL_ROLES (OQ-9): a JSON object from an identity provider id (idp_...)
// or "default" to the ARN of the IAM role that provider federates to for kubectl, for example
//
//	{"default":"arn:aws:iam::111122223333:role/afs-viewers"}
//
// Empty or unset means no provider has a role, so no access entry is ever created. A role that is not an
// IAM role ARN, or a key that is neither "default" nor a provider id, fails startup rather than being
// ignored: a typo must not silently grant or withhold access.
func providerRoles(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("AFS_VIEWER_KUBECTL_ROLES is not a JSON object of strings: %w", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != kubectl.DefaultProvider && !strings.HasPrefix(k, "idp_") {
			return nil, fmt.Errorf("AFS_VIEWER_KUBECTL_ROLES: key %q is neither %q nor an identity provider id", k, kubectl.DefaultProvider)
		}
		if !roleARNRE.MatchString(m[k]) {
			return nil, fmt.Errorf("AFS_VIEWER_KUBECTL_ROLES: the value for %q is not an IAM role ARN", k)
		}
	}
	return m, nil
}

// wireKubectl builds the viewer kubectl controller. Without a cluster name or without any configured role
// the controller still runs and removes any viewer access it finds, so nothing outlives a removed
// configuration.
func wireKubectl(kc *k8s.Client, awsCfg awssdk.Config, cluster string, runner *postgres.TxRunner, pool postgres.Querier2, env func(string) string, _ func() time.Time) (kubectlWiring, error) {
	roles, err := providerRoles(env("AFS_VIEWER_KUBECTL_ROLES"))
	if err != nil {
		return kubectlWiring{}, err
	}
	ctl, err := kubectl.NewController(kubectl.Config{
		Source: postgres.NewKubectlSource(runner), Cluster: k8s.NewKubectlCluster(kc),
		Entries: eks.NewViewerEntries(awsCfg, cluster), ProviderRoles: roles,
	})
	if err != nil {
		return kubectlWiring{}, err
	}
	return kubectlWiring{Controller: ctl, Routes: kubectl.Routes(),
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewTeamLister(l, pool, kubectl.Kind) }}, nil
}
