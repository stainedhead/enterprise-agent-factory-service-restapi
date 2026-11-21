package observability

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestStartServesOpsAndShutsDown(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var buf bytes.Buffer
	rt, err := Start(t.Context(), Config{Service: "afs-billing", Version: "v9", MetricsAddr: addr, LogOutput: &buf,
		Checks: []Check{{Name: "database", Fn: func(context.Context) error { return nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for i := 0; i < 50; i++ {
		res, err := httpGet(t, "http://"+addr+"/metrics")
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `afs_build_info{service="afs-billing",version="v9"} 1`) {
		t.Fatalf("metrics not served: %q", body)
	}
	res, err := httpGet(t, "http://"+addr+"/readyz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("readyz %v %v", res, err)
	}
	_ = res.Body.Close()
	rt.Log.Info("started", "token", "abcdefabcdef")
	if strings.Contains(buf.String(), "abcdefabcdef") || !strings.Contains(buf.String(), `"service":"afs-billing"`) {
		t.Errorf("log = %s", buf.String())
	}
	if err := rt.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if res, err := httpGet(t, "http://"+addr+"/healthz"); err == nil {
		_ = res.Body.Close()
		t.Error("ops server still serving after Shutdown")
	}
}

func TestEnvConfig(t *testing.T) {
	c := EnvConfig("afs-api", "v1", func(k string) string {
		return map[string]string{"AFS_LOG_LEVEL": "debug", "AFS_OTLP_ENDPOINT": "http://x:4318"}[k]
	})
	if c.MetricsAddr != ":9090" || c.LogLevel != "debug" || c.OTLP != "http://x:4318" || c.Service != "afs-api" {
		t.Fatalf("%+v", c)
	}
	if got := EnvConfig("s", "v", func(k string) string {
		if k == "AFS_METRICS_ADDR" {
			return ":1"
		}
		return ""
	}); got.MetricsAddr != ":1" {
		t.Fatal("explicit address ignored")
	}
}

// A composition root that recorded into its own registry while wiring gets that registry served.
func TestStartServesTheSuppliedMetrics(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	m := NewMetrics("afs-reconciler", "v1")
	m.OutboxLag(7, 3*time.Second)
	rt, err := Start(t.Context(), Config{Service: "afs-reconciler", Version: "v1", MetricsAddr: addr, LogOutput: io.Discard, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Shutdown(t.Context()) }()
	if rt.Metrics != m {
		t.Fatal("Start built its own registry")
	}
	var body string
	for i := 0; i < 50; i++ {
		res, err := httpGet(t, "http://"+addr+"/metrics")
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "afs_outbox_pending_events 7") {
		t.Fatalf("supplied registry not served: %q", body)
	}
}
