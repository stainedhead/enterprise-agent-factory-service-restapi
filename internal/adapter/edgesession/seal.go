// Package edgesession seals the sign-in state of afs-edge (AES-256-GCM, two active keys so a key can be
// rotated without failing sign-ins in flight) and builds the session cookie.
package edgesession

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// Sealer implements edge.Sealer. keys[0] seals; every key opens.
type Sealer struct {
	aeads []cipher.AEAD
	rand  io.Reader
}

var _ edge.Sealer = (*Sealer)(nil)

// New returns a Sealer on one or two 32-byte keys (current first, previous second).
func New(keys ...[]byte) (*Sealer, error) {
	if len(keys) == 0 || len(keys) > 2 {
		return nil, errors.New("edgesession: one or two keys are required")
	}
	s := &Sealer{rand: rand.Reader}
	for i, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("edgesession: key %d is %d bytes, want 32", i, len(k))
		}
		b, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		g, err := cipher.NewGCM(b)
		if err != nil {
			return nil, err
		}
		s.aeads = append(s.aeads, g)
	}
	return s, nil
}

// ParseKeys decodes base64 (standard or URL, padded or not) keys.
func ParseKeys(encoded ...string) ([][]byte, error) {
	var out [][]byte
	for i, e := range encoded {
		if e == "" {
			continue
		}
		var b []byte
		var err error
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if b, err = enc.DecodeString(e); err == nil {
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("edgesession: key %d is not base64", i)
		}
		out = append(out, b)
	}
	return out, nil
}

// Seal implements edge.Sealer: nonce || ciphertext, base64url.
func (s *Sealer) Seal(plain []byte) (string, error) {
	g := s.aeads[0]
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(g.Seal(nonce, nonce, plain, nil)), nil
}

// Open implements edge.Sealer.
func (s *Sealer) Open(sealed string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errors.New("edgesession: malformed state")
	}
	for _, g := range s.aeads {
		if len(raw) < g.NonceSize() {
			continue
		}
		if p, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil); err == nil {
			return p, nil
		}
	}
	return nil, errors.New("edgesession: state does not open")
}

// Cookie builds the host-only session cookie: Secure, HttpOnly, SameSite=Lax, no Domain, Path=/.
func Cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// ClearCookie expires the cookie.
func ClearCookie(name string) *http.Cookie { return Cookie(name, "", -1) }
