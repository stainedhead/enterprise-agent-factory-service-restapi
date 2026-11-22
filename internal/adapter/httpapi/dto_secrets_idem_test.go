package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

var (
	idemKeyA = bytes.Repeat([]byte("a"), 32)
	idemKeyB = bytes.Repeat([]byte("b"), 32)
)

func sp(s string) *string { return &s }

// FR-R14 AC1: the value contributes an HMAC under the server key; the old unsalted 32-bit tag is gone and
// the value is not recoverable from the hashed body by hashing guesses.
func TestIdemBodyValueIsKeyedHMAC(t *testing.T) {
	raw := []byte(`{"name":"db","value":"hunter2"}`)
	out, err := idemBody(idemKeyA, raw, sp("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("value in hashed body: %s", out)
	}
	unsalted := sha256.Sum256([]byte("afs-idem-value|hunter2"))
	if strings.Contains(string(out), hex.EncodeToString(unsalted[:4])) {
		t.Fatalf("body still carries the unsalted tag: %s", out)
	}
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if got := m["valueMac"]; len(got) != 64 {
		t.Fatalf("want a full 32-byte HMAC (64 hex), got %q", got)
	}
}

func TestIdemBodyDependsOnKeyAndValue(t *testing.T) {
	raw := []byte(`{"name":"db","value":"v1"}`)
	a1, _ := idemBody(idemKeyA, raw, sp("v1"))
	a2, _ := idemBody(idemKeyA, raw, sp("v1"))
	b, _ := idemBody(idemKeyB, raw, sp("v1"))
	other, _ := idemBody(idemKeyA, []byte(`{"name":"db","value":"v2"}`), sp("v2"))
	if !bytes.Equal(a1, a2) {
		t.Fatal("same key and value must give the same body")
	}
	if bytes.Equal(a1, b) {
		t.Fatal("different server keys must give different bodies (FR-R14 AC3)")
	}
	if bytes.Equal(a1, other) {
		t.Fatal("different values must differ (idempotency mismatch)")
	}
}

// FR-R14 AC2: an unparsable body is an error, never a shared nil hash.
func TestIdemBodyRefusesUnparsable(t *testing.T) {
	for _, raw := range []string{``, `not json`, `[1,2]`, `"x"`, `null`} {
		out, err := idemBody(idemKeyA, []byte(raw), nil)
		if err == nil || out != nil {
			t.Errorf("%q: want a validation error, got %q, %v", raw, out, err)
		}
	}
}

func TestIdemBodyRefusesShortKey(t *testing.T) {
	if _, err := idemBody([]byte("short"), []byte(`{"value":"x"}`), sp("x")); err == nil {
		t.Fatal("a key under 32 bytes must be refused")
	}
}
