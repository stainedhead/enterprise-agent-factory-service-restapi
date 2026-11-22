package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(rune('a'+b)), 32)))
}

func TestSettings(t *testing.T) {
	good := map[string]string{"AFS_DATABASE_URL": "postgres://x", "AFS_EDGE_STATE_KEY": key(1), "AFS_EDGE_TLS_CERT": "/tls/tls.crt", "AFS_EDGE_TLS_KEY": "/tls/tls.key"}
	s, err := settingsFrom(envOf(good))
	if err != nil || s.Addr != ":8443" || s.IdleTimeout != 10*time.Minute || s.RefreshInterval != 30*time.Second || len(s.StateKeys) != 1 || s.DBIAMAuth {
		t.Fatalf("%+v %v", s, err)
	}
	good["AFS_EDGE_STATE_KEY_PREVIOUS"] = key(2)
	good["AFS_EDGE_ADDR"] = ":9000"
	good["AFS_DB_IAM_AUTH"] = "true"
	good["AFS_EDGE_IDLE_TIMEOUT"] = "2m"
	if s, err = settingsFrom(envOf(good)); err != nil || len(s.StateKeys) != 2 || s.Addr != ":9000" || !s.DBIAMAuth || s.IdleTimeout != 2*time.Minute {
		t.Fatalf("%+v %v", s, err)
	}
	for name, mut := range map[string]map[string]string{
		"no database":   {"AFS_DATABASE_URL": ""},
		"no key":        {"AFS_EDGE_STATE_KEY": ""},
		"no cert":       {"AFS_EDGE_TLS_CERT": ""},
		"no tls key":    {"AFS_EDGE_TLS_KEY": ""},
		"short key":     {"AFS_EDGE_STATE_KEY": base64.StdEncoding.EncodeToString([]byte("short"))},
		"not base64":    {"AFS_EDGE_STATE_KEY": "***"},
		"bad bool":      {"AFS_DB_IAM_AUTH": "maybe"},
		"bad duration":  {"AFS_EDGE_FLUSH_INTERVAL": "soon"},
		"zero duration": {"AFS_EDGE_DRAIN_TIMEOUT": "0s"},
	} {
		env := map[string]string{"AFS_DATABASE_URL": "postgres://x", "AFS_EDGE_STATE_KEY": key(1), "AFS_EDGE_TLS_CERT": "c", "AFS_EDGE_TLS_KEY": "k"}
		for k, v := range mut {
			env[k] = v
		}
		if _, err := settingsFrom(envOf(env)); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
}
