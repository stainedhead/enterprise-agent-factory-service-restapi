package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sigv4"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpoauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpup"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// sealingKeys parses AFS_GATEWAY_MCP_SEALING_KEYS: one or two base64 32-byte keys, current first, comma-separated.
func sealingKeys(v string) ([][]byte, error) {
	if v == "" {
		return nil, nil
	}
	var out [][]byte
	for _, p := range strings.Split(v, ",") {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("AFS_GATEWAY_MCP_SEALING_KEYS: each key must be 32 bytes, base64")
		}
		out = append(out, k)
	}
	if len(out) > 2 {
		return nil, fmt.Errorf("AFS_GATEWAY_MCP_SEALING_KEYS: at most two keys")
	}
	return out, nil
}

// mcpApp is the MCP route of a team gateway (gateway contract §13).
type mcpApp struct {
	Family    gatewayhttp.Family
	Admission *mcpgateway.CachedAdmission
	Targets   *mcpup.CachedTargets
	Ledger    *postgres.McpLedger
	Refresher *mcpup.Refresher
	enabled   *mcpEnabled
}

// mcpEnabled caches mcp.enabled for a few seconds; when it is off, or cannot be read, every MCP path is 404.
type mcpEnabled struct {
	store *postgres.McpGatewayStore
	ttl   time.Duration
	mu    sync.Mutex
	at    time.Time
	on    bool
}

func (e *mcpEnabled) get(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.at) < e.ttl {
		return e.on
	}
	on, err := e.store.McpEnabled(ctx)
	if err != nil {
		return false
	}
	e.on, e.at = on, time.Now()
	return on
}

func (e *mcpEnabled) invalidate() {
	e.mu.Lock()
	e.at = time.Time{}
	e.mu.Unlock()
}

// identifyMcp maps a request to its agent or pool with the gateway's identity rules (gateway contract §3).
func identifyMcp(svc *gateway.Service) mcpgateway.Identify {
	return func(r *http.Request) (mcpgateway.Caller, error) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return mcpgateway.Caller{}, err
		}
		c, err := svc.Authenticate(r.Context(), ip.Unmap(), r.Header.Get(gatewayhttp.TokenHeader))
		if err != nil {
			return mcpgateway.Caller{}, err
		}
		out := mcpgateway.Caller{TeamID: c.TeamID, SubjectKind: "agent", SubjectID: c.AgentID}
		if c.PoolID != "" {
			out.SubjectKind, out.SubjectID, out.WorkerPod = "pool", c.PoolID, c.PodName
		}
		return out, nil
	}
}

// wireMcp builds the MCP family of a team gateway. A gateway with no sealing key serves no MCP (every /mcp path
// falls through to the contract's 404).
func wireMcp(ctx context.Context, log *slog.Logger, s settings, pool *pgxpool.Pool, identify mcpgateway.Identify, creds awssdk.CredentialsProvider, region string) (*mcpApp, error) {
	if s.TeamNamespace == "" || len(s.McpSealingKeys) == 0 {
		return nil, nil
	}
	sealer, err := mcpwire.NewSealer(s.McpSealingKeys...)
	if err != nil {
		return nil, err
	}
	store := postgres.NewMcpGatewayStore(pool)
	defaults, searchMax, err := store.McpLimits(ctx)
	if err != nil {
		log.Warn("mcp limits unreadable; using the contract defaults", "error", err.Error())
	}
	app := &mcpApp{
		Admission: mcpgateway.NewCachedAdmission(store, s.CacheTTL, time.Now),
		Targets:   &mcpup.CachedTargets{Next: store},
		Ledger:    postgres.NewMcpLedger(pool, log),
	}
	up := mcpup.New(mcpup.Config{Targets: app.Targets, State: store, Tokens: &mcpoauth.Client{}, Signer: sigv4.New(creds),
		TeamID: s.TeamID, Namespace: s.TeamNamespace, CredentialsDir: s.CredentialsDir, Region: region, Version: buildinfo.Version})
	h, err := mcpgateway.New(mcpgateway.Config{Identify: identify, Admission: app.Admission, Upstream: up, Ledger: app.Ledger,
		Sealer: sealer, Version: buildinfo.Version, Defaults: defaults, SearchLimitMax: searchMax, Log: log})
	if err != nil {
		return nil, err
	}
	enabled := &mcpEnabled{store: store, ttl: 5 * time.Second}
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled.get(r.Context()) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	app.Family = gatewayhttp.FamilyFunc(func(m *gatewayhttp.Mux) {
		m.Handle("/mcp", gated)
		m.Handle("/mcp/", gated)
	})
	app.Refresher = &mcpup.Refresher{Store: store, Upstream: up, TeamID: s.TeamID, Log: log}
	app.enabled = enabled
	return app, nil
}

// Invalidate drops what a change to table makes stale.
func (a *mcpApp) Invalidate(table string) {
	a.Admission.Invalidate(table)
	switch table {
	case "mcp_servers":
		a.Targets.InvalidateAll()
	case "service_settings":
		a.enabled.invalidate()
	}
}

// InvalidateAll drops every cache, after the change feed reconnects.
func (a *mcpApp) InvalidateAll() {
	a.Admission.InvalidateAll()
	a.Targets.InvalidateAll()
	a.enabled.invalidate()
}
