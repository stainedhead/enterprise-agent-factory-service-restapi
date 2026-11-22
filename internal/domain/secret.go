package domain

import (
	"fmt"
	"log/slog"
	"regexp"
	"time"
	"unicode/utf8"
)

// MaxSecretValueBytes is the largest secret value Secrets Manager accepts and the API passes on.
const MaxSecretValueBytes = 64 * 1024

// MaxSecretDescription is the longest secret description.
const MaxSecretDescription = 1000

var secretNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// SecretValue is a secret's value while it passes through the API. It exists only in the create,
// put and rotate request DTOs and the Secrets Manager writer. It never marshals, prints or logs:
// every formatting path answers "[redacted]" and JSON and text encoding fail, so an accidental
// log line, audit record or event cannot carry it. Expose is the single way to read it, for the
// Secrets Manager call.
type SecretValue struct{ v string }

// NewSecretValue validates s as a secret value: 1 byte to 64 KiB. field names the request field.
func NewSecretValue(field, s string) (SecretValue, error) {
	switch {
	case s == "":
		return SecretValue{}, Validation(FieldError{Field: field, Reason: "is required"})
	case len(s) > MaxSecretValueBytes:
		return SecretValue{}, Validation(FieldError{Field: field, Reason: "must be at most 65536 bytes"})
	case !utf8.ValidString(s):
		return SecretValue{}, Validation(FieldError{Field: field, Reason: "must be valid UTF-8 text"})
	}
	return SecretValue{v: s}, nil
}

// Expose returns the value. Only the Secrets Manager adapter calls it.
func (s SecretValue) Expose() string { return s.v }

// IsZero reports whether no value is held.
func (s SecretValue) IsZero() bool { return s.v == "" }

// String implements fmt.Stringer.
func (SecretValue) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer, which %#v uses.
func (SecretValue) GoString() string { return "domain.SecretValue{[redacted]}" }

// Format implements fmt.Formatter so no verb prints the value.
func (SecretValue) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }

// LogValue implements slog.LogValuer.
func (SecretValue) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalJSON refuses: a secret value is never serialised.
func (SecretValue) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("domain: a secret value is never marshalled")
}

// MarshalText refuses, for encoders that look for TextMarshaler.
func (SecretValue) MarshalText() ([]byte, error) {
	return nil, fmt.Errorf("domain: a secret value is never marshalled")
}

// Rotation is a secret's rotation setting. Days is zero when rotation is off.
type Rotation struct {
	Enabled bool
	Days    int
}

// Validate checks the rotation setting: enabled needs 1 to 365 days, disabled none.
func (r Rotation) Validate() error {
	switch {
	case r.Enabled && (r.Days < 1 || r.Days > 365):
		return Validation(FieldError{Field: "rotation.days", Reason: "must be 1 to 365 when rotation is enabled"})
	case !r.Enabled && r.Days != 0:
		return Validation(FieldError{Field: "rotation.days", Reason: "is only for enabled rotation"})
	}
	return nil
}

// Secret is the metadata of a secret: a Secrets Manager reference. It has no value field, ever.
type Secret struct {
	ID               string
	TeamID           string
	Scope            SecretScope
	AgentID          string
	PoolID           string
	Name             string
	Description      string
	ExternalSystemID string
	// Path is the Secrets Manager name the database derived from afs.secret_path.
	Path string
	// ARN is the Secrets Manager ARN.
	ARN           string
	VersionID     string
	LastChangedAt *time.Time
	Rotation      Rotation
	// BoundTo lists the agents that bind the secret.
	BoundTo   []string
	Version   Version
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
}

// ScopeOf returns the scope a stored secret has from its owner columns.
func ScopeOf(agentID, poolID string) SecretScope {
	switch {
	case agentID != "":
		return SecretScopeAgent
	case poolID != "":
		return SecretScopePool
	}
	return SecretScopeTeam
}

// NewSecretParams is a CreateSecret body.
type NewSecretParams struct {
	Scope            SecretScope
	AgentID          string
	PoolID           string
	Name             string
	Description      string
	ExternalSystemID string
	Rotation         Rotation
	Value            SecretValue
}

// Validate checks the shape of a CreateSecret body.
func (p NewSecretParams) Validate() error {
	var fe []FieldError
	add := func(f, r string) { fe = append(fe, FieldError{Field: f, Reason: r}) }
	switch p.Scope {
	case SecretScopeTeam:
		if p.AgentID != "" || p.PoolID != "" {
			add("scope", "a team secret names neither agentId nor poolId")
		}
	case SecretScopeAgent:
		if p.PoolID != "" {
			add("poolId", "is only for pool scope")
		}
		if _, err := ParseID(PrefixAgent, p.AgentID); err != nil {
			add("agentId", "is required for agent scope and must be an agt_ identifier")
		}
	case SecretScopePool:
		if p.AgentID != "" {
			add("agentId", "is only for agent scope")
		}
		if _, err := ParseID(PrefixPool, p.PoolID); err != nil {
			add("poolId", "is required for pool scope and must be a pool_ identifier")
		}
	default:
		add("scope", "must be team, agent or pool")
	}
	if !secretNameRE.MatchString(p.Name) {
		add("name", "must match ^[a-z][a-z0-9-]{0,62}$")
	}
	if utf8.RuneCountInString(p.Description) > MaxSecretDescription {
		add("description", "must be at most 1000 characters")
	}
	if p.ExternalSystemID != "" {
		if _, err := ParseID(PrefixExtSystem, p.ExternalSystemID); err != nil {
			add("externalSystemId", "must be an ext_ identifier")
		}
	}
	if err := p.Rotation.Validate(); err != nil {
		fe = append(fe, err.(*Error).Fields...) //nolint:errcheck,forcetypeassert // Validate returns *Error
	}
	if p.Value.IsZero() {
		add("value", "is required")
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// SecretPatch is an UpdateSecret body. A nil field is left unchanged.
type SecretPatch struct {
	Description *string
	// ExternalSystemID: Set with Null clears the link.
	ExternalSystemSet  bool
	ExternalSystemNull bool
	ExternalSystemID   string
	Rotation           *Rotation
}

// Empty reports whether the patch changes nothing.
func (p SecretPatch) Empty() bool {
	return p.Description == nil && !p.ExternalSystemSet && p.Rotation == nil
}

// Validate checks the patch.
func (p SecretPatch) Validate() error {
	var fe []FieldError
	if p.Empty() {
		fe = append(fe, FieldError{Field: "body", Reason: "names no field to change"})
	}
	if p.Description != nil && utf8.RuneCountInString(*p.Description) > MaxSecretDescription {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
	}
	if p.ExternalSystemSet && !p.ExternalSystemNull {
		if _, err := ParseID(PrefixExtSystem, p.ExternalSystemID); err != nil {
			fe = append(fe, FieldError{Field: "externalSystemId", Reason: "must be an ext_ identifier or null"})
		}
	}
	if p.Rotation != nil {
		if err := p.Rotation.Validate(); err != nil {
			fe = append(fe, err.(*Error).Fields...) //nolint:errcheck,forcetypeassert // Validate returns *Error
		}
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// Apply returns s with the patch applied.
func (p SecretPatch) Apply(s Secret) Secret {
	if p.Description != nil {
		s.Description = *p.Description
	}
	if p.ExternalSystemSet {
		s.ExternalSystemID = p.ExternalSystemID
		if p.ExternalSystemNull {
			s.ExternalSystemID = ""
		}
	}
	if p.Rotation != nil {
		s.Rotation = *p.Rotation
	}
	return s
}

// SecretBound is the Conflict for deleting a secret that agents still bind; the agents are listed
// in details.agentIds.
func SecretBound(agentIDs []string) *Error {
	return Conflict("secret_bound", "the secret is bound to agents; remove the bindings first",
		map[string]any{"agentIds": agentIDs})
}
