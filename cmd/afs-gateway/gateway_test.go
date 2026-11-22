package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSettings(t *testing.T) {
	base := map[string]string{"AFS_DATABASE_URL": "postgres://x", "AFS_GATEWAY_ROLE_ARN": "arn:aws:iam::111122223333:role/gw",
		"AFS_GATEWAY_TLS_CERT": "/tls/c", "AFS_GATEWAY_TLS_KEY": "/tls/k"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := map[string]struct {
		env     map[string]string
		wantErr bool
		check   func(settings) bool
	}{
		"defaults": {with(), false, func(s settings) bool {
			return s.Addr == ":8443" && s.MaxBody == 20<<20 && s.CiliumCheck && s.CacheTTL == time.Minute
		}},
		"overrides": {with("AFS_GATEWAY_ADDR", ":9443", "AFS_GATEWAY_MAX_BODY", "1024", "AFS_GATEWAY_CILIUM_CHECK", "false", "AFS_DB_IAM_AUTH", "true", "AFS_GATEWAY_CACHE_TTL", "5s"), false, func(s settings) bool {
			return s.Addr == ":9443" && s.MaxBody == 1024 && !s.CiliumCheck && s.DBIAMAuth && s.CacheTTL == 5*time.Second
		}},
		"no database":       {with("AFS_DATABASE_URL", ""), true, nil},
		"no role":           {with("AFS_GATEWAY_ROLE_ARN", ""), true, nil},
		"no tls":            {with("AFS_GATEWAY_TLS_KEY", ""), true, nil},
		"bad max body":      {with("AFS_GATEWAY_MAX_BODY", "lots"), true, nil},
		"zero max body":     {with("AFS_GATEWAY_MAX_BODY", "0"), true, nil},
		"bad bool":          {with("AFS_DB_IAM_AUTH", "maybe"), true, nil},
		"bad ttl":           {with("AFS_GATEWAY_CACHE_TTL", "-1s"), true, nil},
		"bad ttl not a dur": {with("AFS_GATEWAY_CACHE_TTL", "x"), true, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := settingsFrom(envOf(c.env))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v", err)
			}
			if c.check != nil && !c.check(s) {
				t.Errorf("settings %+v", s)
			}
		})
	}
}

func TestBedrockEndpoint(t *testing.T) {
	if got := bedrockEndpoint(settings{BedrockURL: "https://vpce.example"}, "us-east-1"); got != "https://vpce.example" {
		t.Error(got)
	}
	if got := bedrockEndpoint(settings{}, "eu-west-1"); got != "https://bedrock-runtime.eu-west-1.amazonaws.com" {
		t.Error(got)
	}
}

func writePair(t *testing.T, dir, cn string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	kb, _ := x509.MarshalECPrivateKey(k)
	_ = os.WriteFile(filepath.Join(dir, "c.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "k.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func cnOf(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(c.Certificate[0])
	return x.Subject.CommonName
}

func TestCertReloaderPicksUpARenewedCertificateAndSurvivesABrokenOne(t *testing.T) {
	dir := t.TempDir()
	writePair(t, dir, "one")
	r, err := newCertReloader(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	r.now = func() time.Time { return clock }
	if cnOf(t, r) != "one" || r.tlsConfig().MinVersion < 0x0303 {
		t.Fatal("first certificate")
	}
	writePair(t, dir, "two")
	if cnOf(t, r) != "one" {
		t.Error("reloaded before the interval")
	}
	clock = clock.Add(2 * time.Minute)
	if cnOf(t, r) != "two" {
		t.Error("did not reload")
	}
	_ = os.WriteFile(filepath.Join(dir, "c.pem"), []byte("garbage"), 0o600)
	clock = clock.Add(2 * time.Minute)
	if cnOf(t, r) != "two" {
		t.Error("a broken file replaced a good certificate")
	}
	if _, err := newCertReloader(filepath.Join(dir, "none"), filepath.Join(dir, "none")); err == nil {
		t.Error("missing files accepted")
	}
}

func TestMetricsObserver(t *testing.T) {
	m := observability.NewMetrics("afs-gateway", "test")
	o := metricsObserver{m}
	o.Done("converse", 200, 3*time.Millisecond)
	o.ReserveFailed("unavailable")
}
