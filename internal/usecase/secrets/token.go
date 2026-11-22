package secrets

import (
	"crypto/sha256"
	"fmt"
)

// tokenOf derives the 36 character, UUID-shaped ClientRequestToken from parts (a retry of the same
// request yields the same token). Parts are length-prefixed so they cannot run together.
func tokenOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	b := h.Sum(nil)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
