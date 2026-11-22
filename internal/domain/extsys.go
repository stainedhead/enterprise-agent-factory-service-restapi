package domain

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	extSlugRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	endpointHost  = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	maxEndpoints  = 50
	maxDocsURLLen = 500
)

// ExtCategory is the category of an external system.
type ExtCategory string

// External system categories. llm is a model provider other than Bedrock: calls to it bypass the
// gateway, so they are not metered or held to the team's hard cap.
const (
	ExtDevTools      ExtCategory = "dev_tools"
	ExtCollaboration ExtCategory = "collaboration"
	ExtEmail         ExtCategory = "email"
	ExtCloud         ExtCategory = "cloud"
	ExtIdentity      ExtCategory = "identity"
	ExtLLM           ExtCategory = "llm"
	ExtOther         ExtCategory = "other"
)

// Valid reports whether c is a known category.
func (c ExtCategory) Valid() bool {
	switch c {
	case ExtDevTools, ExtCollaboration, ExtEmail, ExtCloud, ExtIdentity, ExtLLM, ExtOther:
		return true
	}
	return false
}

// ExternalSystem is an outside service agents may reach: a service-level catalog entry (TeamID
// empty) or one declared by a team. Its endpoints are rendered into each agent's egress policy.
type ExternalSystem struct {
	ID          string
	TeamID      string
	Slug        string
	Name        string
	Category    ExtCategory
	Description string
	DocsURL     string
	Owner       string
	Purpose     string
	Endpoints   []string
	Enabled     bool
	Builtin     bool
	Version     Version
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TeamDeclared reports whether a team declared the system.
func (s ExternalSystem) TeamDeclared() bool { return s.TeamID != "" }

// Unmetered reports whether calls to the system bypass the gateway's metering and caps.
func (s ExternalSystem) Unmetered() bool { return s.Category == ExtLLM }

// ExternalSystemAccess is a system as one team or agent sees it: the effective state and the level
// that switched it off.
type ExternalSystemAccess struct {
	ExternalSystem
	DisabledAt ExtSystemDisabledAt
}

// On reports whether the system is available at the level of the view.
func (a ExternalSystemAccess) On() bool { return a.DisabledAt == "" }

// TeamOn reports whether the team (ignoring the agent level) may use the system.
func (a ExternalSystemAccess) TeamOn() bool {
	return a.DisabledAt != ExtSystemDisabledAtService && a.DisabledAt != ExtSystemDisabledAtTeam
}

// ValidateEndpoints checks a system's endpoints: 1 to 50 entries of host, host:port or *.domain
// with an optional port, lower case, no duplicates (the database's afs.endpoints_ok).
func ValidateEndpoints(field string, eps []string) error {
	if len(eps) == 0 || len(eps) > maxEndpoints {
		return Validation(FieldError{Field: field, Reason: "needs from 1 to 50 endpoints"})
	}
	seen := map[string]bool{}
	for _, e := range eps {
		host, port, hasPort := strings.Cut(e, ":")
		if !endpointHost.MatchString(host) {
			return Validation(FieldError{Field: field, Reason: "endpoint " + strconv.Quote(e) + " must be a lower case host, host:port or *.domain"})
		}
		if hasPort {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
				return Validation(FieldError{Field: field, Reason: "endpoint " + strconv.Quote(e) + " has an invalid port"})
			}
		}
		if seen[e] {
			return Validation(FieldError{Field: field, Reason: "endpoint " + strconv.Quote(e) + " is listed twice"})
		}
		seen[e] = true
	}
	return nil
}

func validateExtCommon(fe []FieldError, slug *string, name *string, cat *ExtCategory, desc, docs *string, eps []string, epsSet bool) []FieldError {
	if slug != nil && !extSlugRE.MatchString(*slug) {
		fe = append(fe, FieldError{Field: "slug", Reason: "must match ^[a-z][a-z0-9-]{1,62}$"})
	}
	if name != nil && (strings.TrimSpace(*name) == "" || len(*name) > 100) {
		fe = append(fe, FieldError{Field: "name", Reason: "is required and at most 100 characters"})
	}
	if cat != nil && !cat.Valid() {
		fe = append(fe, FieldError{Field: "category", Reason: "must be dev_tools, collaboration, email, cloud, identity, llm or other"})
	}
	if desc != nil && len(*desc) > 2000 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 2000 characters"})
	}
	if docs != nil && *docs != "" && (!strings.HasPrefix(*docs, "https://") || len(*docs) > maxDocsURLLen) {
		fe = append(fe, FieldError{Field: "docsUrl", Reason: "must be an https:// URL of at most 500 characters"})
	}
	if epsSet {
		if err := ValidateEndpoints("endpoints", eps); err != nil {
			fe = append(fe, err.(*Error).Fields...)
		}
	}
	return fe
}

// DeclareSystemParams are the fields of DeclareTeamExternalSystem. A team-declared system needs an
// owner and a purpose, and cannot reuse a catalog slug (checked against the catalog by the use case).
type DeclareSystemParams struct {
	Slug        string
	Name        string
	Category    ExtCategory
	Description string
	DocsURL     string
	Endpoints   []string
	Owner       string
	Purpose     string
}

// NewTeamSystem validates p and returns the system for teamID.
func NewTeamSystem(teamID string, p DeclareSystemParams) (ExternalSystem, error) {
	fe := validateExtCommon(nil, &p.Slug, &p.Name, &p.Category, &p.Description, &p.DocsURL, p.Endpoints, true)
	if strings.TrimSpace(p.Owner) == "" || len(p.Owner) > 200 {
		fe = append(fe, FieldError{Field: "owner", Reason: "is required for a team-declared system (at most 200 characters)"})
	}
	if strings.TrimSpace(p.Purpose) == "" || len(p.Purpose) > 1000 {
		fe = append(fe, FieldError{Field: "purpose", Reason: "is required for a team-declared system (at most 1000 characters)"})
	}
	if len(fe) > 0 {
		return ExternalSystem{}, Validation(fe...)
	}
	return ExternalSystem{TeamID: teamID, Slug: p.Slug, Name: p.Name, Category: p.Category, Description: p.Description, DocsURL: p.DocsURL,
		Endpoints: p.Endpoints, Owner: strings.TrimSpace(p.Owner), Purpose: strings.TrimSpace(p.Purpose), Enabled: true}, nil
}

// SystemUpdate are the optional fields of an update; nil leaves a field as it is. Owner and Purpose
// apply to team-declared systems only, Enabled to catalog systems only (the service-level switch).
type SystemUpdate struct {
	Name        *string
	Description *string
	DocsURL     *string
	Endpoints   *[]string
	Owner       *string
	Purpose     *string
	Enabled     *bool
}

func (u SystemUpdate) empty() bool {
	return u.Name == nil && u.Description == nil && u.DocsURL == nil && u.Endpoints == nil && u.Owner == nil && u.Purpose == nil && u.Enabled == nil
}

// Apply returns s with u applied. A team-declared system keeps a non-empty owner and purpose.
func (s ExternalSystem) Apply(u SystemUpdate) (ExternalSystem, error) {
	if u.empty() {
		return ExternalSystem{}, Validation(FieldError{Field: "body", Reason: "at least one field is required"})
	}
	var eps []string
	if u.Endpoints != nil {
		eps = *u.Endpoints
	}
	fe := validateExtCommon(nil, nil, u.Name, nil, u.Description, u.DocsURL, eps, u.Endpoints != nil)
	if s.TeamDeclared() {
		if u.Owner != nil && (strings.TrimSpace(*u.Owner) == "" || len(*u.Owner) > 200) {
			fe = append(fe, FieldError{Field: "owner", Reason: "must not be empty (at most 200 characters)"})
		}
		if u.Purpose != nil && (strings.TrimSpace(*u.Purpose) == "" || len(*u.Purpose) > 1000) {
			fe = append(fe, FieldError{Field: "purpose", Reason: "must not be empty (at most 1000 characters)"})
		}
	} else {
		if u.Owner != nil {
			fe = append(fe, FieldError{Field: "owner", Reason: "applies to team-declared systems only"})
		}
		if u.Purpose != nil {
			fe = append(fe, FieldError{Field: "purpose", Reason: "applies to team-declared systems only"})
		}
	}
	if len(fe) > 0 {
		return ExternalSystem{}, Validation(fe...)
	}
	if u.Name != nil {
		s.Name = *u.Name
	}
	if u.Description != nil {
		s.Description = *u.Description
	}
	if u.DocsURL != nil {
		s.DocsURL = *u.DocsURL
	}
	if u.Endpoints != nil {
		s.Endpoints = *u.Endpoints
	}
	if u.Owner != nil {
		s.Owner = strings.TrimSpace(*u.Owner)
	}
	if u.Purpose != nil {
		s.Purpose = strings.TrimSpace(*u.Purpose)
	}
	if u.Enabled != nil {
		s.Enabled = *u.Enabled
	}
	return s, nil
}
