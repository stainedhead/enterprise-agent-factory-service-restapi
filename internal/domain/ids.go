package domain

import (
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"time"
)

// crockford is the ULID alphabet (Crockford base32, upper case, no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidLen is the length of an encoded ULID.
const ulidLen = 26

// IDPrefix is the type prefix of a server-generated identifier (API contract §1).
type IDPrefix string

// Identifier prefixes of API contract §1 and the database's afs.ulid_ok checks.
const (
	PrefixTeam            IDPrefix = "team"
	PrefixAgent           IDPrefix = "agt"
	PrefixPool            IDPrefix = "pool"
	PrefixStage           IDPrefix = "stg"
	PrefixSecret          IDPrefix = "sec"
	PrefixBinding         IDPrefix = "rb"
	PrefixAPIKey          IDPrefix = "key"
	PrefixUser            IDPrefix = "usr"
	PrefixIdP             IDPrefix = "idp"
	PrefixExtSystem       IDPrefix = "ext"
	PrefixRoleLink        IDPrefix = "arl"
	PrefixPoolRoleLink    IDPrefix = "prl"
	PrefixAgentIdentity   IDPrefix = "aid"
	PrefixNamespaceConfig IDPrefix = "ncv"
	PrefixAgentConfig     IDPrefix = "acv"
	PrefixPoolConfig      IDPrefix = "pcv"
	PrefixBudget          IDPrefix = "bud"
	PrefixModelPrice      IDPrefix = "mpp"
	PrefixSnapshot        IDPrefix = "vsnap"
	PrefixWorkOrder       IDPrefix = "wo"
	PrefixSwarmTemplate   IDPrefix = "swt"
	PrefixExport          IDPrefix = "exp"
	PrefixReportSchedule  IDPrefix = "rsc"
	PrefixLogDestination  IDPrefix = "log"
	PrefixChannel         IDPrefix = "nch"
	PrefixEgressRule      IDPrefix = "egr"
	PrefixExtSystemOptout IDPrefix = "xso"
	PrefixAgentTemplate   IDPrefix = "tpl"
)

// AllIDPrefixes lists every prefix, in API contract §1 order followed by tpl.
func AllIDPrefixes() []IDPrefix {
	return []IDPrefix{
		PrefixTeam, PrefixAgent, PrefixPool, PrefixStage, PrefixSecret, PrefixBinding, PrefixAPIKey,
		PrefixUser, PrefixIdP, PrefixExtSystem, PrefixRoleLink, PrefixPoolRoleLink, PrefixAgentIdentity,
		PrefixNamespaceConfig, PrefixAgentConfig, PrefixPoolConfig, PrefixBudget, PrefixModelPrice,
		PrefixSnapshot, PrefixWorkOrder, PrefixSwarmTemplate, PrefixExport, PrefixReportSchedule,
		PrefixLogDestination, PrefixChannel, PrefixEgressRule, PrefixExtSystemOptout, PrefixAgentTemplate,
	}
}

// ID is a prefixed ULID such as team_01JD0000000000000000000000.
type ID string

// String returns the identifier text.
func (id ID) String() string { return string(id) }

// Prefix returns the identifier's prefix, or "" when it has none.
func (id ID) Prefix() IDPrefix {
	p, _, ok := strings.Cut(string(id), "_")
	if !ok {
		return ""
	}
	return IDPrefix(p)
}

// NewID returns a new identifier with prefix p, time t and randomness from entropy
// (crypto/rand when nil).
func NewID(p IDPrefix, t time.Time, entropy io.Reader) (ID, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	var b [16]byte
	ms := uint64(t.UnixMilli()) //nolint:gosec // ULID time is the unsigned millisecond count
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := io.ReadFull(entropy, b[6:]); err != nil {
		return "", fmt.Errorf("new %s id: %w", p, err)
	}
	return ID(string(p) + "_" + encodeULID(b)), nil
}

// encodeULID encodes 128 bits as 26 base32 characters (the first carries 3 bits).
func encodeULID(b [16]byte) string {
	out := make([]byte, ulidLen)
	var acc uint64
	bits := 0
	pos := ulidLen - 1
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 && pos >= 0 {
			out[pos] = crockford[acc&31]
			acc >>= 5
			bits -= 5
			pos--
		}
	}
	if pos == 0 {
		out[0] = crockford[acc&31]
	}
	return string(out)
}

// ParseID validates s as an identifier with prefix p. The body must be a 26 character Crockford
// base32 ULID in upper case, as afs.ulid_ok requires.
func ParseID(p IDPrefix, s string) (ID, error) {
	rest, ok := strings.CutPrefix(s, string(p)+"_")
	if !ok || !ulidBodyOK(rest) {
		return "", Validation(FieldError{Field: "id", Reason: fmt.Sprintf("must be a %s_ identifier", p)})
	}
	return ID(s), nil
}

func ulidBodyOK(s string) bool {
	if len(s) != ulidLen || s[0] > '7' {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(crockford, c) {
			return false
		}
	}
	return true
}
