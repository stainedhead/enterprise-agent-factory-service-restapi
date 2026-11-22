package edgesession

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

type failRand struct{}

func (failRand) Read([]byte) (int, error) { return 0, errors.New("rand") }

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealOpen(t *testing.T) {
	s, err := New(key(1), key(2))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal([]byte("state"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.Open(sealed); err != nil || string(p) != "state" {
		t.Fatalf("%q %v", p, err)
	}
	// a sealer whose previous key is the old current key still opens it (rotation)
	rot, _ := New(key(9), key(1))
	if p, err := rot.Open(sealed); err != nil || string(p) != "state" {
		t.Fatalf("rotation: %q %v", p, err)
	}
	other, _ := New(key(7))
	if _, err := other.Open(sealed); err == nil {
		t.Error("an unrelated key must not open it")
	}
	tampered := []byte(sealed)
	tampered[len(tampered)-2] ^= 1
	if _, err := s.Open(string(tampered)); err == nil {
		t.Error("tampering must fail")
	}
	for _, bad := range []string{"%%%", "", "abc"} {
		if _, err := s.Open(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	s.rand = failRand{}
	if _, err := s.Seal([]byte("x")); err == nil {
		t.Error("rand failure")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(); err == nil {
		t.Error("no keys")
	}
	if _, err := New(key(1), key(2), key(3)); err == nil {
		t.Error("three keys")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("short key")
	}
}

func TestParseKeys(t *testing.T) {
	k := key(5)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		got, err := ParseKeys(enc.EncodeToString(k), "")
		if err != nil || len(got) != 1 || !bytes.Equal(got[0], k) {
			t.Fatalf("%v %v", got, err)
		}
	}
	if _, err := ParseKeys("***"); err == nil {
		t.Error("not base64")
	}
}

func TestCookie(t *testing.T) {
	c := Cookie("__Host-afs-collab", "v", 60)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" || c.MaxAge != 60 {
		t.Fatalf("%+v", c)
	}
	if ClearCookie("n").MaxAge >= 0 {
		t.Error("clear must expire")
	}
}
