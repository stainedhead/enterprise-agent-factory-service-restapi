package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func host() func() (string, error) { return func() (string, error) { return "pod-0", nil } }

func TestSettingsFrom(t *testing.T) {
	base := map[string]string{"AFS_DATABASE_URL": "postgres://x", "AFS_GATEWAY_ROLE_ARN": "arn:aws:iam::111122223333:role/gw", "AFS_RECONCILER_USERS": " a , b,,",
		"AFS_EVENT_BUS_NAME": "afs-events", "AFS_CLUSTER_NAME": "afs-dev"}
	s, err := settingsFrom(envOf(base), host())
	if err != nil {
		t.Fatal(err)
	}
	if s.MetricsAddr != ":9090" || s.LeaseNamespace != "afs-system" || s.PodName != "pod-0" || len(s.ReconcilerUsers) != 2 || s.ReconcilerUsers[1] != "b" || s.GatewayName != "afs-gateway" {
		t.Fatalf("defaults wrong: %+v", s)
	}
	cases := map[string]struct {
		mut  func(map[string]string)
		want string
	}{
		"no db":          {func(m map[string]string) { delete(m, "AFS_DATABASE_URL") }, "AFS_DATABASE_URL"},
		"no gw":          {func(m map[string]string) { delete(m, "AFS_GATEWAY_ROLE_ARN") }, "AFS_GATEWAY_ROLE_ARN"},
		"no bus":         {func(m map[string]string) { delete(m, "AFS_EVENT_BUS_NAME") }, "AFS_EVENT_BUS_NAME"},
		"no cluster":     {func(m map[string]string) { delete(m, "AFS_CLUSTER_NAME") }, "AFS_CLUSTER_NAME"},
		"bad iam":        {func(m map[string]string) { m["AFS_DB_IAM_AUTH"] = "maybe" }, "AFS_DB_IAM_AUTH"},
		"no users":       {func(m map[string]string) { delete(m, "AFS_RECONCILER_USERS") }, "AFS_RECONCILER_USERS"},
		"bad poll":       {func(m map[string]string) { m["AFS_POLL_INTERVAL"] = "soon" }, "AFS_POLL_INTERVAL"},
		"neg":            {func(m map[string]string) { m["AFS_RESYNC_INTERVAL"] = "-1s" }, "AFS_RESYNC_INTERVAL"},
		"bad gw db port": {func(m map[string]string) { m["AFS_TEAM_GATEWAY_DB_PORT"] = "x" }, "AFS_TEAM_GATEWAY_DB_PORT"},
		"short mcp key":  {func(m map[string]string) { m["AFS_TEAM_GATEWAY_MCP_SESSION_KEYS"] = "c2hvcnQ=" }, "AFS_TEAM_GATEWAY_MCP_SESSION_KEYS"},
		"three mcp keys": {func(m map[string]string) {
			m["AFS_TEAM_GATEWAY_MCP_SESSION_KEYS"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
		}, "at most two"},
	}
	for name, tc := range cases {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		tc.mut(m)
		if _, err := settingsFrom(envOf(m), host()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %s", name, err, tc.want)
		}
	}
	iam := map[string]string{"AFS_DB_IAM_AUTH": "true"}
	for k, v := range base {
		iam[k] = v
	}
	if s, err := settingsFrom(envOf(iam), host()); err != nil || !s.DBIAMAuth || s.EventBusName != "afs-events" {
		t.Errorf("IAM auth: %+v %v", s, err)
	}
	tg := map[string]string{"AFS_TEAM_GATEWAY_IMAGE": "registry.example.com/afs-gateway:1.2.0", "AFS_TEAM_GATEWAY_DB_PORT": "5433",
		"AFS_TEAM_GATEWAY_DB_HOST": "db.example.com", "AFS_TEAM_GATEWAY_TLS_ISSUER": "afs-internal",
		"AFS_TEAM_GATEWAY_MCP_SESSION_KEYS": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "AFS_MCP_BRIDGE_IMAGE": "registry.example.com/afs-mcp-bridge:1.3.0"}
	for k, v := range base {
		tg[k] = v
	}
	if s, err := settingsFrom(envOf(tg), host()); err != nil || s.TeamGateway.DatabasePort != 5433 || s.TeamGateway.Image == "" || s.TeamGateway.TLSIssuer != "afs-internal" ||
		len(s.TeamGateway.McpSessionKeys) != 2 || s.TeamGateway.McpBridgeImage == "" {
		t.Errorf("team gateway: %+v %v", s.TeamGateway, err)
	}
	if _, err := settingsFrom(envOf(base), func() (string, error) { return "", errors.New("no") }); err == nil {
		t.Error("without POD_NAME the hostname is the identity; its failure must be reported")
	}
	m := map[string]string{"POD_NAME": "p"}
	for k, v := range base {
		m[k] = v
	}
	m["AFS_POLL_INTERVAL"] = "5s"
	if s, err := settingsFrom(envOf(m), func() (string, error) { return "", errors.New("no") }); err != nil || s.PodName != "p" || s.PollInterval != 5*time.Second {
		t.Errorf("s=%+v err=%v", s, err)
	}
}
