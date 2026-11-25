package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// settings are read from the environment (architecture §7, user-docs/configuration.md).
type settings struct {
	DatabaseURL    string // AFS_DATABASE_URL: the afs_reconciler login
	MetricsAddr    string // AFS_METRICS_ADDR, default :9090
	LeaseNamespace string // AFS_LEADER_LEASE_NAMESPACE
	PodName        string // POD_NAME or the hostname: the leader election identity
	Kubeconfig     string // AFS_KUBECONFIG: out-of-cluster development only

	DBIAMAuth              bool     // AFS_DB_IAM_AUTH: sign an RDS IAM token as the database password
	EventBusName           string   // AFS_EVENT_BUS_NAME: the EventBridge bus the outbox publishes to
	ClusterName            string   // AFS_CLUSTER_NAME
	GatewayRoleARN         string   // AFS_GATEWAY_ROLE_ARN
	PermissionsBoundaryARN string   // AFS_PERMISSIONS_BOUNDARY_ARN
	ESORoleARN             string   // AFS_ESO_ROLE_ARN (optional)
	KMSKeyARN              string   // AFS_KMS_KEY_ARN (optional): the Secrets Manager key, for the collaboration agent's role
	SecretsPrefix          string   // AFS_SECRETS_PREFIX, default afs
	ReconcilerUsers        []string // AFS_RECONCILER_USERS: Kubernetes usernames of this service
	GatewayNamespace       string   // AFS_GATEWAY_NAMESPACE, default afs-system
	GatewayName            string   // AFS_GATEWAY_NAME, default afs-gateway
	GatewayCAConfigMap     string   // AFS_GATEWAY_CA_CONFIGMAP, default afs-gateway-ca (in the gateway namespace)
	EdgeNamespace          string   // AFS_EDGE_NAMESPACE, default afs-system: where afs-edge runs (collaboration ingress)
	EdgeName               string   // AFS_EDGE_NAME, default afs-edge
	// TeamGateway configures per-namespace gateways (gateway contract §12.1): AFS_TEAM_GATEWAY_IMAGE,
	// AFS_TEAM_GATEWAY_DB_SECRET_ARN, AFS_TEAM_GATEWAY_DB_HOST, AFS_TEAM_GATEWAY_DB_PORT, AFS_TEAM_GATEWAY_TLS_ISSUER,
	// and for MCP (1.3.0) AFS_TEAM_GATEWAY_MCP_SESSION_KEYS and AFS_MCP_BRIDGE_IMAGE.
	TeamGateway    team.TeamGatewaySpec
	PollInterval   time.Duration
	ResyncInterval time.Duration
}

func settingsFrom(env func(string) string, hostname func() (string, error)) (settings, error) {
	s := settings{
		DatabaseURL: env("AFS_DATABASE_URL"), MetricsAddr: env("AFS_METRICS_ADDR"), LeaseNamespace: env("AFS_LEADER_LEASE_NAMESPACE"),
		PodName: env("POD_NAME"), Kubeconfig: env("AFS_KUBECONFIG"), ClusterName: env("AFS_CLUSTER_NAME"), EventBusName: env("AFS_EVENT_BUS_NAME"),
		GatewayRoleARN: env("AFS_GATEWAY_ROLE_ARN"), PermissionsBoundaryARN: env("AFS_PERMISSIONS_BOUNDARY_ARN"),
		ESORoleARN: env("AFS_ESO_ROLE_ARN"), KMSKeyARN: env("AFS_KMS_KEY_ARN"), SecretsPrefix: env("AFS_SECRETS_PREFIX"),
		GatewayNamespace: env("AFS_GATEWAY_NAMESPACE"), GatewayName: env("AFS_GATEWAY_NAME"), GatewayCAConfigMap: env("AFS_GATEWAY_CA_CONFIGMAP"),
		EdgeNamespace: env("AFS_EDGE_NAMESPACE"), EdgeName: env("AFS_EDGE_NAME"),
		TeamGateway: team.TeamGatewaySpec{Image: env("AFS_TEAM_GATEWAY_IMAGE"), DatabaseSecretARN: env("AFS_TEAM_GATEWAY_DB_SECRET_ARN"),
			DatabaseHost: env("AFS_TEAM_GATEWAY_DB_HOST"), TLSIssuer: env("AFS_TEAM_GATEWAY_TLS_ISSUER"), McpBridgeImage: env("AFS_MCP_BRIDGE_IMAGE")},
	}
	if v := env("AFS_TEAM_GATEWAY_MCP_SESSION_KEYS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			if err != nil || len(k) != 32 {
				return s, errors.New("AFS_TEAM_GATEWAY_MCP_SESSION_KEYS: each key must be 32 bytes, base64")
			}
			s.TeamGateway.McpSessionKeys = append(s.TeamGateway.McpSessionKeys, k)
		}
		if len(s.TeamGateway.McpSessionKeys) > 2 {
			return s, errors.New("AFS_TEAM_GATEWAY_MCP_SESSION_KEYS: at most two keys")
		}
	}
	if v := env("AFS_TEAM_GATEWAY_DB_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return s, fmt.Errorf("AFS_TEAM_GATEWAY_DB_PORT: %q is not a port", v)
		}
		s.TeamGateway.DatabasePort = p
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&s.MetricsAddr, ":9090")
	def(&s.LeaseNamespace, "afs-system")
	def(&s.SecretsPrefix, "afs")
	def(&s.GatewayNamespace, "afs-system")
	def(&s.GatewayName, "afs-gateway")
	def(&s.GatewayCAConfigMap, "afs-gateway-ca")
	def(&s.EdgeNamespace, "afs-system")
	def(&s.EdgeName, "afs-edge")
	if s.PodName == "" {
		h, err := hostname()
		if err != nil || h == "" {
			return s, errors.New("POD_NAME is not set and the hostname is unavailable")
		}
		s.PodName = h
	}
	for _, u := range strings.Split(env("AFS_RECONCILER_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			s.ReconcilerUsers = append(s.ReconcilerUsers, u)
		}
	}
	if v := env("AFS_DB_IAM_AUTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("AFS_DB_IAM_AUTH: %q is not a boolean", v)
		}
		s.DBIAMAuth = b
	}
	for name, d := range map[string]*time.Duration{"AFS_POLL_INTERVAL": &s.PollInterval, "AFS_RESYNC_INTERVAL": &s.ResyncInterval} {
		if v := env(name); v != "" {
			p, err := time.ParseDuration(v)
			if err != nil || p <= 0 {
				return s, fmt.Errorf("%s: %q is not a positive duration", name, v)
			}
			*d = p
		}
	}
	var missing []string
	for name, v := range map[string]string{"AFS_DATABASE_URL": s.DatabaseURL, "AFS_GATEWAY_ROLE_ARN": s.GatewayRoleARN,
		"AFS_EVENT_BUS_NAME": s.EventBusName, "AFS_CLUSTER_NAME": s.ClusterName} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(s.ReconcilerUsers) == 0 {
		missing = append(missing, "AFS_RECONCILER_USERS")
	}
	if len(missing) > 0 {
		return s, fmt.Errorf("missing required settings: %s", strings.Join(sorted(missing), ", "))
	}
	return s, nil
}

func sorted(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}
