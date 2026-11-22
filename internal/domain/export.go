package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BundleSchemaVersion is the schema version of an export bundle: the generation of the desired-state
// model the bundle is read from (migration 000014 introduced bundles). It rises when the content of a
// bundle changes shape; an importer refuses a bundle newer than it understands.
const BundleSchemaVersion = 14

// MaxBundleBytes is the largest import body (RA-12: 100 MB).
const MaxBundleBytes = 100 << 20

// ExportRetention is how long a pre-signed download link of an export stays valid (RC-20).
const ExportRetention = 7 * 24 * time.Hour

// Export is one export_bundles row of direction export: the request and, once the reconciler built
// it, the bundle's checksum, size and download link.
type Export struct {
	ID            string
	Status        ExportStatus
	SchemaVersion int
	// TeamIDs are the teams requested; empty means every team.
	TeamIDs   []string
	CreatedAt time.Time
	CreatedBy string
	// FinishedAt is set once the export succeeded or failed.
	FinishedAt *time.Time
	// SHA256 is the checksum of the bundle's canonical content; nil until succeeded.
	SHA256    *string
	SizeBytes *int64
	S3URI     *string
	// DownloadURL is a pre-signed link valid until DownloadExpiresAt (7 days).
	DownloadURL       *string
	DownloadExpiresAt *time.Time
	// Error is the failure summary of a failed export.
	Error *string
}

// ExpiredAt reports whether the download link has expired at now: after that a new export is needed.
func (e Export) ExpiredAt(now time.Time) bool {
	return e.DownloadExpiresAt != nil && !now.Before(*e.DownloadExpiresAt)
}

// ValidateExportTeams checks the team ids of a CreateExport request and removes duplicates.
func ValidateExportTeams(ids []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	var fields []FieldError
	for i, id := range ids {
		if _, err := ParseID(PrefixTeam, id); err != nil {
			fields = append(fields, FieldError{Field: fmt.Sprintf("teamIds[%d]", i), Reason: "must be a team id"})
			continue
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(fields) > 0 {
		return nil, Validation(fields...)
	}
	sort.Strings(out)
	return out, nil
}

// Row is one database row of a bundle, keyed by column name. Numbers are json.Number so a value
// round-trips through canonical JSON unchanged.
type Row map[string]any

// Bundle is the export bundle of root CFG-5: the desired state of the exported teams and the service
// level external systems catalog. It never holds a secret value, an API key hash or a client secret:
// secrets are Secrets Manager references and metadata only.
//
// Row fields use the database's column names. The checksum covers the canonical JSON of the bundle
// without its sha256 member.
type Bundle struct {
	SchemaVersion int    `json:"schemaVersion"`
	SHA256        string `json:"sha256,omitempty"`
	// ExportedAt is when the bundle was built (RFC 3339).
	ExportedAt string `json:"exportedAt"`
	// ExternalSystems is the service level catalog.
	ExternalSystems []Row        `json:"externalSystems"`
	Teams           []TeamBundle `json:"teams"`
}

// TeamBundle is one team's desired state.
type TeamBundle struct {
	// Team holds the team's settings, including slug.
	Team   Row      `json:"team"`
	Models []string `json:"models"`
	Tags   []Row    `json:"tags"`
	// Budget is nil when the team has none.
	Budget          Row   `json:"budget,omitempty"`
	Members         []Row `json:"members"`
	NamespaceConfig []Row `json:"namespaceConfig"`
	// ExternalSystems are the systems the team declared; Optouts are its team level switches.
	ExternalSystems []Row         `json:"externalSystems"`
	Optouts         []Row         `json:"externalSystemOptouts"`
	Secrets         []Row         `json:"secrets"`
	Agents          []AgentBundle `json:"agents"`
	// Collaboration holds the team's collaboration settings (web_ui_enabled, rest_api_enabled,
	// registration_mode) and the name of its collaboration agent; nil when the team has none (COL-26).
	// It never carries Paperclip data, a Paperclip id or a secret value.
	Collaboration Row `json:"collaboration,omitempty"`
}

// AgentBundle is one agent with everything that hangs off it.
type AgentBundle struct {
	Agent          Row   `json:"agent"`
	ConfigValues   []Row `json:"configValues"`
	SecretBindings []Row `json:"secretBindings"`
	Identities     []Row `json:"identities"`
	RoleLinks      []Row `json:"roleLinks"`
	Optouts        []Row `json:"externalSystemOptouts"`
	// Collaboration holds a worker's connect choice ({connect}); nil when it has no row. The link state and
	// the Paperclip agent id are observed, not desired, and are never exported.
	Collaboration Row `json:"collaboration,omitempty"`
	// LatestSnapshot references the agent's newest completed volume snapshot; nil when none.
	LatestSnapshot Row `json:"latestSnapshot,omitempty"`
}

// Slug returns the team's slug.
func (t TeamBundle) Slug() string {
	s, _ := t.Team["slug"].(string)
	return s
}

// forbiddenBundleKeys are member names a bundle never carries (RA-US-14.1). Matching is
// case-insensitive and ignores underscores and hyphens.
var forbiddenBundleKeys = map[string]bool{
	"keysha256": true, "keyhash": true, "apikeyhash": true, "clientsecret": true, "secretvalue": true,
	"secretstring": true, "secretbinary": true, "password": true, "plaintext": true, "privatekey": true,
	"accesstoken": true, "refreshtoken": true, "apikey": true, "authorization": true, "token": true,
}

func normalizeKey(k string) string {
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "_", "")
	return strings.ReplaceAll(k, "-", "")
}

// ForbiddenBundleKey returns the path of the first member in v whose name must never appear in a
// bundle, or "". v is decoded JSON (maps, slices and scalars).
func ForbiddenBundleKey(v any) string { return forbiddenKey(v, "$") }

func forbiddenKey(v any, path string) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if forbiddenBundleKeys[normalizeKey(k)] {
				return path + "." + k
			}
			if p := forbiddenKey(x[k], path+"."+k); p != "" {
				return p
			}
		}
	case Row:
		return forbiddenKey(map[string]any(x), path)
	case []any:
		for i, e := range x {
			if p := forbiddenKey(e, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

// CanonicalJSON encodes v as canonical JSON: members sorted by name, no insignificant whitespace,
// UTF-8 without HTML escaping, numbers exactly as written. Two equal documents have equal bytes.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return CanonicalizeJSON(raw)
}

// CanonicalizeJSON rewrites a JSON document in canonical form.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON document")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(x.String())
	case string:
		return writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", v)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return nil
}

// Checksum returns the bundle's checksum and its canonical content. The checksum is the lowercase
// hex SHA-256 of the canonical JSON of the bundle without its sha256 member; the returned document is
// the canonical JSON of the bundle with sha256 set, which is what an export stores and an import
// accepts.
func (b Bundle) Checksum() (sum string, document []byte, err error) {
	b.SHA256 = ""
	body, err := CanonicalJSON(b)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256(body)
	sum = hex.EncodeToString(h[:])
	b.SHA256 = sum
	document, err = CanonicalJSON(b)
	return sum, document, err
}

// Seal checks that the bundle carries nothing forbidden and returns its checksum and stored document.
// A builder calls it on every bundle before writing it anywhere.
func (b Bundle) Seal() (sum string, document []byte, err error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return "", nil, err
	}
	if p := ForbiddenBundleKey(tree); p != "" {
		return "", nil, fmt.Errorf("bundle member %s must never be exported", p)
	}
	return b.Checksum()
}

// ParseBundle reads a bundle document from an import request and checks it: it must be a JSON object
// whose sha256 member equals the checksum of its content, whose schemaVersion this build understands,
// and that carries no secret value, key hash or client secret. Every failure is a ValidationError and
// nothing may be written after one (RA-US-14.4).
func ParseBundle(raw []byte) (Bundle, error) {
	bad := func(field, reason string) (Bundle, error) {
		return Bundle{}, Validation(FieldError{Field: field, Reason: reason})
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree map[string]any
	if err := dec.Decode(&tree); err != nil {
		return bad("bundle", "must be a JSON object")
	}
	declared, _ := tree["sha256"].(string)
	if len(declared) != 64 || strings.ToLower(declared) != declared {
		return bad("bundle.sha256", "must be the 64 character lowercase hex SHA-256 of the bundle")
	}
	delete(tree, "sha256")
	body, err := CanonicalJSON(tree)
	if err != nil {
		return bad("bundle", "is not valid JSON")
	}
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != declared {
		return bad("bundle.sha256", "does not match the content of the bundle")
	}
	if p := ForbiddenBundleKey(tree); p != "" {
		return bad("bundle", "must not carry "+p+": secret values, key hashes and client secrets are never exported")
	}
	var out Bundle
	strict := json.NewDecoder(bytes.NewReader(body))
	strict.UseNumber()
	strict.DisallowUnknownFields()
	if err := strict.Decode(&out); err != nil {
		return bad("bundle", "does not have the shape of an export bundle: "+err.Error())
	}
	switch {
	case out.SchemaVersion < 1:
		return bad("bundle.schemaVersion", "is required")
	case out.SchemaVersion > BundleSchemaVersion:
		return bad("bundle.schemaVersion", fmt.Sprintf("%d is newer than this service understands (%d)", out.SchemaVersion, BundleSchemaVersion))
	}
	seen := map[string]bool{}
	for i, t := range out.Teams {
		slug := t.Slug()
		if slug == "" {
			return bad(fmt.Sprintf("bundle.teams[%d]", i), "needs team.slug")
		}
		if seen[slug] {
			return bad(fmt.Sprintf("bundle.teams[%d]", i), "team "+slug+" appears twice")
		}
		seen[slug] = true
	}
	out.SHA256 = declared
	return out, nil
}
