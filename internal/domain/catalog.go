package domain

import (
	"regexp"
	"strings"
	"time"
)

var (
	storageClassNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	harnessRE          = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)
	templateDigestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	cpuQuantityRE      = regexp.MustCompile(`^([0-9]+m|[0-9]+(\.[0-9]+)?)$`)
	memQuantityRE      = regexp.MustCompile(`^[0-9]+(Ki|Mi|Gi|Ti|K|M|G|T)?$`)
)

// VolumeType is the EBS type behind a storage class; only block storage is offered to agents.
type VolumeType string

// Volume types.
const (
	VolumeTypeGP3 VolumeType = "gp3"
	VolumeTypeIO2 VolumeType = "io2"
)

// Valid reports whether t is an offered volume type.
func (t VolumeType) Valid() bool { return t == VolumeTypeGP3 || t == VolumeTypeIO2 }

// StorageClass is a Kubernetes storage class a service admin offers for agent volumes. The name is
// the class name the reconciler uses and cannot change.
type StorageClass struct {
	Name        string
	Description string
	VolumeType  VolumeType
	Enabled     bool
	Version     Version
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewStorageClass validates the fields of PutStorageClass. enabled defaults to true.
func NewStorageClass(name, description string, vt VolumeType, enabled *bool) (StorageClass, error) {
	var fe []FieldError
	if !storageClassNameRE.MatchString(name) {
		fe = append(fe, FieldError{Field: "name", Reason: "must match ^[a-z][a-z0-9-]{0,62}$"})
	}
	if !vt.Valid() {
		fe = append(fe, FieldError{Field: "volumeType", Reason: "must be gp3 or io2"})
	}
	if len(description) > 500 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 500 characters"})
	}
	if len(fe) > 0 {
		return StorageClass{}, Validation(fe...)
	}
	sc := StorageClass{Name: name, Description: description, VolumeType: vt, Enabled: true}
	if enabled != nil {
		sc.Enabled = *enabled
	}
	return sc, nil
}

// Resources are the CPU and memory of an agent, as Kubernetes quantities.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Validate checks the quantities; both are optional.
func (r Resources) Validate(field string) []FieldError {
	var fe []FieldError
	if r.CPU != "" && !cpuQuantityRE.MatchString(r.CPU) {
		fe = append(fe, FieldError{Field: field + ".cpu", Reason: "must be a CPU quantity such as 1, 0.5 or 500m"})
	}
	if r.Memory != "" && !memQuantityRE.MatchString(r.Memory) {
		fe = append(fe, FieldError{Field: field + ".memory", Reason: "must be a memory quantity such as 2Gi"})
	}
	return fe
}

// AgentTemplate is an Agentic Teammates template teams may use (AGT-11, D54). Disabling a template
// or removing a digest blocks new agents and pools with it and changes no existing one.
type AgentTemplate struct {
	ID               string
	Name             string
	Description      string
	Harness          string
	DefaultResources Resources
	AllowedDigests   []string
	CurrentDigest    string
	Enabled          bool
	// Capabilities are the known capabilities of the image; paperclip marks a collaboration template.
	Capabilities []string
	Version      Version
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PutAgentTemplateParams are the fields of PutAgentTemplate. Enabled defaults to true.
type PutAgentTemplateParams struct {
	Name             string
	Description      string
	Harness          string
	DefaultResources Resources
	AllowedDigests   []string
	CurrentDigest    string
	Enabled          *bool
	Capabilities     []string
}

// NewAgentTemplate validates p; the id is set by the caller on create.
func NewAgentTemplate(p PutAgentTemplateParams) (AgentTemplate, error) {
	var fe []FieldError
	if !nameRE.MatchString(p.Name) {
		fe = append(fe, FieldError{Field: "name", Reason: "must match ^[a-z][a-z0-9-]{1,38}[a-z0-9]$"})
	}
	if !harnessRE.MatchString(p.Harness) {
		fe = append(fe, FieldError{Field: "harness", Reason: "must be kebab-case, for example hermes"})
	}
	if len(p.Description) > 1000 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
	}
	fe = append(fe, p.DefaultResources.Validate("defaultResources")...)
	switch {
	case len(p.AllowedDigests) == 0 || len(p.AllowedDigests) > 50:
		fe = append(fe, FieldError{Field: "allowedDigests", Reason: "needs from 1 to 50 digests"})
	default:
		seen := map[string]bool{}
		for _, d := range p.AllowedDigests {
			if !templateDigestRE.MatchString(d) {
				fe = append(fe, FieldError{Field: "allowedDigests", Reason: "each must be sha256:<64 lowercase hex>"})
				break
			}
			if seen[d] {
				fe = append(fe, FieldError{Field: "allowedDigests", Reason: "must not repeat a digest"})
				break
			}
			seen[d] = true
		}
		if !templateDigestRE.MatchString(p.CurrentDigest) {
			fe = append(fe, FieldError{Field: "currentDigest", Reason: "must be sha256:<64 lowercase hex>"})
		} else if !seen[p.CurrentDigest] {
			fe = append(fe, FieldError{Field: "currentDigest", Reason: "must be one of allowedDigests"})
		}
	}
	if len(p.Capabilities) > 8 {
		fe = append(fe, FieldError{Field: "capabilities", Reason: "at most 8"})
	}
	fe = append(fe, validateCapabilities(p.Capabilities)...)
	if len(fe) > 0 {
		return AgentTemplate{}, Validation(fe...)
	}
	caps := append([]string{}, p.Capabilities...)
	t := AgentTemplate{Capabilities: caps, Name: p.Name, Description: strings.TrimSpace(p.Description), Harness: p.Harness, DefaultResources: p.DefaultResources,
		AllowedDigests: append([]string(nil), p.AllowedDigests...), CurrentDigest: p.CurrentDigest, Enabled: true}
	if p.Enabled != nil {
		t.Enabled = *p.Enabled
	}
	return t, nil
}

// AllowsDigest reports whether digest (sha256:<hex>) is on the template's allowed list.
func (t AgentTemplate) AllowsDigest(digest string) bool {
	for _, d := range t.AllowedDigests {
		if d == digest {
			return true
		}
	}
	return false
}

// ValidateCapabilityFilter checks the ?capability= filter of the agent template lists.
func ValidateCapabilityFilter(v string) error {
	if v != "" && !knownCapability(v) {
		return Validation(FieldError{Field: "capability", Reason: "must be " + knownCapabilityList})
	}
	return nil
}

const knownCapabilityList = "orchestration, tool:paperclip, tool:buzz or paperclip (deprecated)"

func knownCapability(c string) bool {
	switch c {
	case CapabilityOrchestration, CapabilityToolPaperclip, CapabilityToolBuzz, CapabilityPaperclip:
		return true
	}
	return false
}

// HasOrchestrationCapability reports whether a template's capabilities mark it as an orchestration image
// (orchestration, or the deprecated paperclip).
func HasOrchestrationCapability(caps []string) bool {
	for _, c := range caps {
		if c == CapabilityOrchestration || c == CapabilityPaperclip {
			return true
		}
	}
	return false
}

// validateCapabilities checks the capabilities of a PutAgentTemplate body: known values, no repeats, at most one
// tool, and a tool only with orchestration.
func validateCapabilities(caps []string) []FieldError {
	var fe []FieldError
	seen := map[string]bool{}
	tools := 0
	for _, cp := range caps {
		switch {
		case !knownCapability(cp):
			fe = append(fe, FieldError{Field: "capabilities", Reason: "unknown capability " + cp + " (known: " + knownCapabilityList + ")"})
		case seen[cp]:
			fe = append(fe, FieldError{Field: "capabilities", Reason: "must not repeat " + cp})
		}
		seen[cp] = true
		if cp == CapabilityToolPaperclip || cp == CapabilityToolBuzz || cp == CapabilityPaperclip {
			tools++
		}
	}
	if tools > 1 {
		fe = append(fe, FieldError{Field: "capabilities", Reason: "name exactly one tool capability"})
	}
	if (seen[CapabilityToolPaperclip] || seen[CapabilityToolBuzz]) && !seen[CapabilityOrchestration] {
		fe = append(fe, FieldError{Field: "capabilities", Reason: "a tool capability needs orchestration"})
	}
	return fe
}
