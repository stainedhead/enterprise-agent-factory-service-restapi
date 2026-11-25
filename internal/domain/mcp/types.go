package mcp

import (
	"errors"
	"regexp"
	"strings"
)

// Kind is the kind of an MCP primitive.
type Kind string

// Primitive kinds.
const (
	KindTool             Kind = "tool"
	KindResource         Kind = "resource"
	KindResourceTemplate Kind = "resource_template"
	KindPrompt           Kind = "prompt"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindTool, KindResource, KindResourceTemplate, KindPrompt:
		return true
	}
	return false
}

// Annotations are the tool hints a server declares. Absent hints take the MCP defaults (readOnly false,
// destructive true, idempotent false, openWorld true), so the fields are pointers. They are
// self-declared: policy by annotation class applies only once a review has confirmed them (PRD §7.3).
type Annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// ReadOnly reports whether the tool declares it does not modify its environment.
func (a Annotations) ReadOnly() bool { return a.ReadOnlyHint != nil && *a.ReadOnlyHint }

// Destructive reports whether the tool may perform destructive updates. A read-only tool never is.
func (a Annotations) Destructive() bool {
	return !a.ReadOnly() && (a.DestructiveHint == nil || *a.DestructiveHint)
}

// Idempotent reports whether repeated calls with the same arguments have no extra effect.
func (a Annotations) Idempotent() bool { return a.IdempotentHint != nil && *a.IdempotentHint }

// OpenWorld reports whether the tool may interact with an open world of external entities.
func (a Annotations) OpenWorld() bool { return a.OpenWorldHint == nil || *a.OpenWorldHint }

// Hint returns a pointer to b, for building Annotations.
func Hint(b bool) *bool { return &b }

// Status is the review status of a primitive's definition.
type Status string

// Primitive statuses.
const (
	StatusApproved      Status = "approved"
	StatusPendingReview Status = "pending_review"
	StatusBlocked       Status = "blocked"
)

// Primitive is one tool, resource, resource template or prompt of a server, as inventoried.
type Primitive struct {
	Server      string
	Kind        Kind
	Name        string // tool or prompt name, resource URI or URI template
	Title       string
	Description string
	Params      []string // input parameter names (tools) or argument names (prompts)
	Annotations Annotations
	Status      Status
}

// QualifiedName is the name an agent sees on the aggregated endpoint: "<server>__<name>".
func (p Primitive) QualifiedName() string { return QualifiedName(p.Server, p.Name) }

// QualifiedName joins a server slug and a primitive name.
func QualifiedName(server, name string) string { return server + Separator + name }

// Separator joins a server slug and a primitive name in qualified names.
const Separator = "__"

// SplitQualifiedName splits "<server>__<name>". Server slugs never contain "__", so the first separator
// is the boundary.
func SplitQualifiedName(q string) (server, name string, ok bool) {
	i := strings.Index(q, Separator)
	if i <= 0 || i+len(Separator) >= len(q) {
		return "", "", false
	}
	return q[:i], q[i+len(Separator):], true
}

var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// Slug validation errors.
var (
	ErrSlugInvalid  = errors.New("slug_invalid")
	ErrSlugReserved = errors.New("slug_reserved")
)

// ValidateSlug checks a server slug: a DNS-label-like name of 2 to 31 characters, no "--" (which could form
// the qualified-name separator with an underscore-free name), not reserved.
func ValidateSlug(s string) error {
	if !slugRE.MatchString(s) || strings.Contains(s, "--") || strings.HasSuffix(s, "-") {
		return ErrSlugInvalid
	}
	if s == "afs" {
		return ErrSlugReserved
	}
	return nil
}

// EnvName turns a slug into the upper-snake form used in AFS_MCP_<SERVER>_URL.
func EnvName(slug string) string {
	return strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))
}
