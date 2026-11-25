package mcpwire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Session is what a 2025-11-25 Mcp-Session-Id carries (gateway contract §13.2). It is sealed, so any
// gateway replica can serve the session with no shared state.
type Session struct {
	Version      string          `json:"v"`
	Subject      string          `json:"s"` // agent or pool id
	Capabilities json.RawMessage `json:"c,omitempty"`
	Expires      int64           `json:"e"` // unix seconds
}

// Session errors. The gateway answers both with HTTP 404 so the client re-initialises.
var (
	ErrSessionInvalid = errors.New("mcpwire: session id invalid")
	ErrSessionExpired = errors.New("mcpwire: session expired")
)

// Sealer seals and opens session ids with AES-256-GCM. keys[0] seals; every key opens, so a key can be
// rotated without dropping live sessions.
type Sealer struct {
	aeads []cipher.AEAD
	rand  io.Reader
	now   func() time.Time
}

// NewSealer returns a Sealer on one or two 32-byte keys (current first).
func NewSealer(keys ...[]byte) (*Sealer, error) {
	if len(keys) == 0 || len(keys) > 2 {
		return nil, errors.New("mcpwire: one or two session keys are required")
	}
	s := &Sealer{rand: rand.Reader, now: time.Now}
	for i, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("mcpwire: session key %d is %d bytes, want 32", i, len(k))
		}
		b, _ := aes.NewCipher(k) // cannot fail for a 32-byte key
		g, _ := cipher.NewGCM(b)
		s.aeads = append(s.aeads, g)
	}
	return s, nil
}

// Seal returns the session id for s. The subject is also the additional data, so an id cannot be
// replayed by another agent.
func (s *Sealer) Seal(sess Session) (string, error) {
	pt, err := json.Marshal(sess)
	if err != nil {
		return "", err
	}
	g := s.aeads[0]
	nonce := make([]byte, g.NonceSize(), g.NonceSize()+len(pt)+g.Overhead())
	if _, err := io.ReadFull(s.rand, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(g.Seal(nonce, nonce, pt, []byte(sess.Subject))), nil
}

// Open checks a session id for subject and returns its contents.
func (s *Sealer) Open(id, subject string) (Session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return Session{}, ErrSessionInvalid
	}
	for _, g := range s.aeads {
		if len(raw) < g.NonceSize() {
			continue
		}
		pt, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], []byte(subject))
		if err != nil {
			continue
		}
		var sess Session
		if json.Unmarshal(pt, &sess) != nil || sess.Subject != subject {
			return Session{}, ErrSessionInvalid
		}
		if s.now().Unix() >= sess.Expires {
			return Session{}, ErrSessionExpired
		}
		return sess, nil
	}
	return Session{}, ErrSessionInvalid
}
