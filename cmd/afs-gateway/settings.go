package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// Mount paths the reconciler renders into a team gateway pod (MEG-RC-4).
const (
	DefaultCredentialsDir  = "/etc/afs/gateway-credentials"          //nolint:gosec // a mount path
	DefaultFederationToken = "/var/run/secrets/afs/federation/token" //nolint:gosec // a mount path, not a credential
)

// settings are read from the environment (architecture §7).
type settings struct {
	Addr        string // AFS_GATEWAY_ADDR, default :8443
	TLSCert     string // AFS_GATEWAY_TLS_CERT
	TLSKey      string // AFS_GATEWAY_TLS_KEY
	BedrockURL  string // AFS_BEDROCK_VPCE_URL; empty means the regional Bedrock Runtime endpoint
	MaxBody     int64  // AFS_GATEWAY_MAX_BODY in bytes, default 20 MB
	DatabaseURL string // AFS_DATABASE_URL: the afs_gateway login
	DBIAMAuth   bool   // AFS_DB_IAM_AUTH
	RoleARN     string // AFS_GATEWAY_ROLE_ARN: the gateway's own role; names the account
	Kubeconfig  string // AFS_KUBECONFIG: out-of-cluster development only
	CacheTTL    time.Duration
	// CiliumCheck cross-checks a pod's IP against Cilium's endpoint identity (D57); AFS_GATEWAY_CILIUM_CHECK.
	CiliumCheck bool
	// Team mode (gateway contract §12.1): set by --team-namespace or AFS_GATEWAY_TEAM_NAMESPACE, with the
	// team's id in AFS_GATEWAY_TEAM_ID. A team gateway serves its namespace only, and model routes.
	TeamNamespace string
	TeamID        string
	// CredentialsDir holds the mounted route credentials, one file per route (AFS_GATEWAY_CREDENTIALS_DIR).
	CredentialsDir string
	// FederationToken is the projected ServiceAccount token exchanged for Azure and Google tokens
	// (AFS_GATEWAY_FEDERATION_TOKEN).
	FederationToken string
}

func settingsFrom(env func(string) string) (settings, error) {
	s := settings{
		Addr: env("AFS_GATEWAY_ADDR"), TLSCert: env("AFS_GATEWAY_TLS_CERT"), TLSKey: env("AFS_GATEWAY_TLS_KEY"),
		BedrockURL: env("AFS_BEDROCK_VPCE_URL"), DatabaseURL: env("AFS_DATABASE_URL"), RoleARN: env("AFS_GATEWAY_ROLE_ARN"),
		Kubeconfig: env("AFS_KUBECONFIG"), MaxBody: gateway.MaxBodyBytes, CacheTTL: time.Minute, CiliumCheck: true,
		TeamNamespace: env("AFS_GATEWAY_TEAM_NAMESPACE"), TeamID: env("AFS_GATEWAY_TEAM_ID"),
		CredentialsDir: env("AFS_GATEWAY_CREDENTIALS_DIR"), FederationToken: env("AFS_GATEWAY_FEDERATION_TOKEN"),
	}
	if s.CredentialsDir == "" {
		s.CredentialsDir = DefaultCredentialsDir
	}
	if s.FederationToken == "" {
		s.FederationToken = DefaultFederationToken
	}
	if s.Addr == "" {
		s.Addr = ":8443"
	}
	if v := env("AFS_GATEWAY_MAX_BODY"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return s, fmt.Errorf("AFS_GATEWAY_MAX_BODY: %q is not a positive number of bytes", v)
		}
		s.MaxBody = n
	}
	for name, dst := range map[string]*bool{"AFS_DB_IAM_AUTH": &s.DBIAMAuth, "AFS_GATEWAY_CILIUM_CHECK": &s.CiliumCheck} {
		if v := env(name); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return s, fmt.Errorf("%s: %q is not a boolean", name, v)
			}
			*dst = b
		}
	}
	if v := env("AFS_GATEWAY_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return s, fmt.Errorf("AFS_GATEWAY_CACHE_TTL: %q is not a positive duration", v)
		}
		s.CacheTTL = d
	}
	switch {
	case s.DatabaseURL == "":
		return s, errors.New("AFS_DATABASE_URL is not set")
	case s.RoleARN == "":
		return s, errors.New("AFS_GATEWAY_ROLE_ARN is not set")
	case (s.TeamNamespace == "") != (s.TeamID == ""):
		return s, errors.New("a team gateway needs both --team-namespace (AFS_GATEWAY_TEAM_NAMESPACE) and AFS_GATEWAY_TEAM_ID")
	case s.TeamID != "" && !domain.ValidTeamID(s.TeamID):
		return s, fmt.Errorf("AFS_GATEWAY_TEAM_ID: %q is not a team id", s.TeamID)
	case s.TLSCert == "" || s.TLSKey == "":
		return s, errors.New("AFS_GATEWAY_TLS_CERT and AFS_GATEWAY_TLS_KEY are required: agents reach the gateway over TLS")
	}
	return s, nil
}
