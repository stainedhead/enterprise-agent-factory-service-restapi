package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgehttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgesession"
)

// settings are read from the environment.
type settings struct {
	Addr        string // AFS_EDGE_ADDR, default :8443 (the ALB forwards HTTPS to this port, DP-3)
	TLSCert     string // AFS_EDGE_TLS_CERT
	TLSKey      string // AFS_EDGE_TLS_KEY
	DatabaseURL string // AFS_DATABASE_URL: the afs_edge login
	DBIAMAuth   bool   // AFS_DB_IAM_AUTH
	// StateKeys are the AEAD keys for the sign-in state: AFS_EDGE_STATE_KEY (current) and
	// AFS_EDGE_STATE_KEY_PREVIOUS (kept so a rotation does not fail sign-ins in flight). Base64, 32 bytes.
	StateKeys       [][]byte
	IdleTimeout     time.Duration // AFS_EDGE_IDLE_TIMEOUT, default 10 m
	RefreshInterval time.Duration // AFS_EDGE_REFRESH_INTERVAL, default 30 s
	FlushInterval   time.Duration // AFS_EDGE_FLUSH_INTERVAL, default 30 s
	DrainTimeout    time.Duration // AFS_EDGE_DRAIN_TIMEOUT, default 30 s
}

func duration(env func(string) string, name string, def time.Duration) (time.Duration, error) {
	v := env(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive duration", name, v)
	}
	return d, nil
}

func settingsFrom(env func(string) string) (settings, error) {
	s := settings{Addr: env("AFS_EDGE_ADDR"), DatabaseURL: env("AFS_DATABASE_URL"), TLSCert: env("AFS_EDGE_TLS_CERT"), TLSKey: env("AFS_EDGE_TLS_KEY")}
	if s.Addr == "" {
		s.Addr = ":8443"
	}
	if v := env("AFS_DB_IAM_AUTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("AFS_DB_IAM_AUTH: %q is not a boolean", v)
		}
		s.DBIAMAuth = b
	}
	var err error
	for _, d := range []struct {
		name string
		def  time.Duration
		dst  *time.Duration
	}{
		{"AFS_EDGE_IDLE_TIMEOUT", edgehttp.DefaultIdleTimeout, &s.IdleTimeout},
		{"AFS_EDGE_REFRESH_INTERVAL", 30 * time.Second, &s.RefreshInterval},
		{"AFS_EDGE_FLUSH_INTERVAL", 30 * time.Second, &s.FlushInterval},
		{"AFS_EDGE_DRAIN_TIMEOUT", 30 * time.Second, &s.DrainTimeout},
	} {
		if *d.dst, err = duration(env, d.name, d.def); err != nil {
			return s, err
		}
	}
	if s.DatabaseURL == "" {
		return s, errors.New("AFS_DATABASE_URL is not set")
	}
	if s.TLSCert == "" || s.TLSKey == "" {
		return s, errors.New("AFS_EDGE_TLS_CERT and AFS_EDGE_TLS_KEY are required: the ALB reaches the edge over HTTPS")
	}
	if env("AFS_EDGE_STATE_KEY") == "" {
		return s, errors.New("AFS_EDGE_STATE_KEY is not set: it seals the sign-in state")
	}
	if s.StateKeys, err = edgesession.ParseKeys(env("AFS_EDGE_STATE_KEY"), env("AFS_EDGE_STATE_KEY_PREVIOUS")); err != nil {
		return s, errors.New("AFS_EDGE_STATE_KEY: not base64")
	}
	if _, err := edgesession.New(s.StateKeys...); err != nil {
		return s, errors.New("AFS_EDGE_STATE_KEY: must be 32 bytes, base64 encoded")
	}
	return s, nil
}
