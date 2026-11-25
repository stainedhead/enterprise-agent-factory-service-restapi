package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunUsageAndVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, func(string) string { return "" }, &out, &errb, nil); code != 0 || !strings.Contains(out.String(), "afs-mcp-bridge") {
		t.Errorf("version: %d %q", code, out.String())
	}
	if code := run(context.Background(), nil, func(string) string { return "" }, &out, &errb, nil); code != exitUsage {
		t.Errorf("no command: %d", code)
	}
	if code := run(context.Background(), []string{"--nope"}, func(string) string { return "" }, &out, &errb, nil); code != exitUsage {
		t.Errorf("bad flag: %d", code)
	}
	if code := run(context.Background(), []string{"--listen", "256.0.0.1:x", "--", "cat"}, func(string) string { return "" }, &out, &errb, nil); code != exitFailed {
		t.Errorf("bad listen: %d", code)
	}
}

func TestRunServes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan int, 1)
	var errb bytes.Buffer
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	go func() {
		done <- run(ctx, []string{"--listen", "127.0.0.1:0", "--", "cat"}, func(k string) string { return env[k] }, &bytes.Buffer{}, &errb, ready)
	}()
	addr := <-ready
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge never became healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d: %s", code, errb.String())
	}
}

func TestChildEnv(t *testing.T) {
	env := map[string]string{"PATH": "/bin", "HOME": "/h", "TOKEN": "x", "OTHER": "y"}
	got := childEnv(" TOKEN, ,MISSING", func(k string) string { return env[k] })
	if want := []string{"PATH=/bin", "HOME=/h", "TOKEN=x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	var out, errb strings.Builder
	if code := run(t.Context(), []string{"--install", dir}, func(string) string { return "" }, &out, &errb, nil); code != 0 {
		t.Fatalf("install = %d %s", code, errb.String())
	}
	st, err := os.Stat(filepath.Join(dir, "afs-mcp-bridge"))
	if err != nil || st.Mode().Perm()&0o111 == 0 || st.Size() == 0 {
		t.Errorf("installed = %v %v", st, err)
	}
	if code := run(t.Context(), []string{"--install", filepath.Join(dir, "missing", "x")}, func(string) string { return "" }, &out, &errb, nil); code != exitFailed {
		t.Errorf("missing dir = %d", code)
	}
}
