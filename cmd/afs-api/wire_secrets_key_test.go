package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestIdempotencyHMACKey(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	long := strings.Repeat("k", 40)
	t.Setenv("AFS_IDEMPOTENCY_HMAC_KEY", long)
	if got := idempotencyHMACKey(log); !bytes.Equal(got, []byte(long)) {
		t.Fatal("a configured key must be used as is")
	}
	for _, v := range []string{"", "short"} {
		t.Setenv("AFS_IDEMPOTENCY_HMAC_KEY", v)
		a, b := idempotencyHMACKey(log), idempotencyHMACKey(log)
		if len(a) < 32 || bytes.Equal(a, b) {
			t.Fatalf("%q: want a random key of at least 32 bytes per call", v)
		}
	}
}
