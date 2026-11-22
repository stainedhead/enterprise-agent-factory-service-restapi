package domain

import (
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Quotas are the ResourceQuota settings of a team (API contract §5). Zero values are unset.
type Quotas struct {
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Pods    int    `json:"pods,omitempty"`
	Storage string `json:"storage,omitempty"`
}

var (
	cpuQuotaRE     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?m?$`)
	storageQuotaRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi)?$`)
	awsAccountRE   = regexp.MustCompile(`^[0-9]{12}$`)
)

// Validate checks the quantities Kubernetes would parse.
func (q Quotas) Validate() error {
	var fe []FieldError
	if q.CPU != "" && !cpuQuotaRE.MatchString(q.CPU) {
		fe = append(fe, FieldError{Field: "quotas.cpu", Reason: "must be a Kubernetes CPU quantity such as 16 or 500m"})
	}
	if q.Memory != "" && !storageQuotaRE.MatchString(q.Memory) {
		fe = append(fe, FieldError{Field: "quotas.memory", Reason: "must be a Kubernetes quantity such as 64Gi"})
	}
	if q.Storage != "" && !storageQuotaRE.MatchString(q.Storage) {
		fe = append(fe, FieldError{Field: "quotas.storage", Reason: "must be a Kubernetes quantity such as 500Gi"})
	}
	if q.Pods < 0 {
		fe = append(fe, FieldError{Field: "quotas.pods", Reason: "must be positive"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// Team is the team aggregate (API contract §5). Status is a read model the reconciler writes.
type Team struct {
	ID                       string
	Slug                     Name
	DisplayName              string
	Description              string
	Kind                     TeamKind
	State                    TeamState
	Namespace                string
	CostCenter               string
	OwnerEmail               string
	MaxBudget                Money
	CeilingOverride          *Money
	CeilingOverrideExpiresAt *time.Time
	Quotas                   Quotas
	IsolationTier            IsolationTier
	ViewerKubectl            bool
	ExpiresAt                *time.Time
	TeardownRule             TeardownRule
	TornDownAt               *time.Time
	TeardownReason           TeardownReason
	IdentityProviderID       string
	AllowedAwsAccounts       []string
	MayManageAwsAccounts     bool
	MayAddExternalSystems    bool
	// MayExposeCollaboration is the guardrail that lets the team publish its Paperclip endpoints (default true).
	MayExposeCollaboration bool
	Version                Version
	Generation             Generation
	CreatedAt              time.Time
	CreatedBy              string
	UpdatedAt              time.Time
	Status                 Status
}

// EffectiveCeiling is the ceiling a budget may reach at now: the override while it is unexpired,
// else maxBudgetUsd (API contract §4.2, D34).
func (t Team) EffectiveCeiling(now time.Time) Money {
	if t.CeilingOverride != nil && t.CeilingOverrideExpiresAt != nil && t.CeilingOverrideExpiresAt.After(now) {
		return *t.CeilingOverride
	}
	return t.MaxBudget
}

// NewTeamParams are the validated-on-construction fields of CreateTeam.
type NewTeamParams struct {
	Slug                  string
	DisplayName           string
	Description           string
	Kind                  TeamKind
	CostCenter            string
	OwnerEmail            string
	MaxBudget             Money
	Quotas                Quotas
	IsolationTier         IsolationTier
	ViewerKubectl         bool
	ExpiresAt             *time.Time
	TeardownRule          TeardownRule
	IdentityProviderID    string
	AllowedAwsAccounts    []string
	MayManageAwsAccounts  bool
	MayAddExternalSystems bool
	// MayExposeCollaboration is the guardrail; nil means the default, true.
	MayExposeCollaboration *bool
}

// NewTeam builds a team in state active, enforcing the invariants of API contract §4.2: an ephemeral
// team needs expiresAt (in the future, at most ephemeralMaxDays ahead) and a teardown rule; a
// persistent team has neither.
func NewTeam(id string, p NewTeamParams, now time.Time, ephemeralMaxDays int) (Team, error) {
	var fe []FieldError
	slug, err := ParseName("slug", p.Slug)
	if err != nil {
		fe = append(fe, FieldError{Field: "slug", Reason: "must match ^[a-z][a-z0-9-]{1,38}[a-z0-9]$"})
	}
	fe = append(fe, validateTeamText(p.DisplayName, p.Description, p.CostCenter, p.OwnerEmail)...)
	if !p.Kind.Valid() {
		fe = append(fe, FieldError{Field: "kind", Reason: "must be persistent or ephemeral"})
	}
	if p.MaxBudget.IsNegative() {
		fe = append(fe, FieldError{Field: "maxBudgetUsd", Reason: "must not be negative"})
	}
	if p.IsolationTier == "" {
		p.IsolationTier = IsolationTierShared
	}
	if !p.IsolationTier.Valid() {
		fe = append(fe, FieldError{Field: "isolationTier", Reason: "must be shared or dedicated"})
	}
	if err := p.Quotas.Validate(); err != nil {
		fe = append(fe, FieldErrors(err)...)
	}
	fe = append(fe, validateAccounts(p.AllowedAwsAccounts)...)
	switch p.Kind {
	case TeamKindEphemeral:
		switch {
		case p.ExpiresAt == nil:
			fe = append(fe, FieldError{Field: "expiresAt", Reason: "is required for an ephemeral team"})
		case !p.ExpiresAt.After(now):
			fe = append(fe, FieldError{Field: "expiresAt", Reason: "must be in the future"})
		case p.ExpiresAt.After(now.Add(time.Duration(ephemeralMaxDays) * 24 * time.Hour)):
			fe = append(fe, FieldError{Field: "expiresAt", Reason: "is more than teams.ephemeral_max_days ahead"})
		}
		if !p.TeardownRule.Valid() {
			fe = append(fe, FieldError{Field: "teardownRule", Reason: "is required for an ephemeral team (at_expiry, at_cap_or_expiry or at_work_complete)"})
		}
	case TeamKindPersistent:
		if p.ExpiresAt != nil {
			fe = append(fe, FieldError{Field: "expiresAt", Reason: "is only for an ephemeral team"})
		}
		if p.TeardownRule != "" {
			fe = append(fe, FieldError{Field: "teardownRule", Reason: "is only for an ephemeral team"})
		}
	}
	if len(fe) > 0 {
		return Team{}, Validation(fe...)
	}
	t := Team{
		ID: id, Slug: slug, DisplayName: p.DisplayName, Description: p.Description, Kind: p.Kind,
		State: TeamStateActive, Namespace: "afs-t-" + string(slug), CostCenter: p.CostCenter, OwnerEmail: p.OwnerEmail,
		MaxBudget: p.MaxBudget, Quotas: p.Quotas, IsolationTier: p.IsolationTier, ViewerKubectl: p.ViewerKubectl,
		ExpiresAt: p.ExpiresAt, TeardownRule: p.TeardownRule, IdentityProviderID: p.IdentityProviderID,
		AllowedAwsAccounts: slices.Clone(p.AllowedAwsAccounts), MayManageAwsAccounts: p.MayManageAwsAccounts,
		MayAddExternalSystems: p.MayAddExternalSystems, MayExposeCollaboration: p.MayExposeCollaboration == nil || *p.MayExposeCollaboration,
		Version: 1, Generation: 1,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Status: PendingStatus(now),
	}
	if t.AllowedAwsAccounts == nil {
		t.AllowedAwsAccounts = []string{}
	}
	return t, nil
}

// FieldErrors returns the field errors of a ValidationError, or one entry for another error.
func FieldErrors(err error) []FieldError {
	if e, ok := err.(*Error); ok && len(e.Fields) > 0 { //nolint:errorlint // domain errors are never wrapped here
		return e.Fields
	}
	return []FieldError{{Field: "body", Reason: err.Error()}}
}

func validateTeamText(displayName, description, costCenter, ownerEmail string) []FieldError {
	var fe []FieldError
	if n := len([]rune(displayName)); n < 1 || n > 100 {
		fe = append(fe, FieldError{Field: "displayName", Reason: "must be 1 to 100 characters"})
	}
	if len([]rune(description)) > 1000 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
	}
	if costCenter == "" || len(costCenter) > 100 {
		fe = append(fe, FieldError{Field: "costCenter", Reason: "is required (1 to 100 characters)"})
	}
	if a, err := mail.ParseAddress(ownerEmail); err != nil || a.Address != ownerEmail {
		fe = append(fe, FieldError{Field: "ownerEmail", Reason: "must be an email address"})
	}
	return fe
}

func validateAccounts(accounts []string) []FieldError {
	var fe []FieldError
	if len(accounts) > 50 {
		fe = append(fe, FieldError{Field: "allowedAwsAccounts", Reason: "at most 50 accounts"})
	}
	seen := map[string]bool{}
	for _, a := range accounts {
		switch {
		case !awsAccountRE.MatchString(a):
			fe = append(fe, FieldError{Field: "allowedAwsAccounts", Reason: a + " is not a 12 digit account id"})
		case seen[a]:
			fe = append(fe, FieldError{Field: "allowedAwsAccounts", Reason: a + " is listed twice"})
		}
		seen[a] = true
	}
	return fe
}

// OptionalTime distinguishes an absent JSON member (Set false) from null (Set true, Value nil) and
// a value, for PATCH bodies.
type OptionalTime struct {
	Set   bool
	Value *time.Time
}

// OptionalMoney is OptionalTime for money.
type OptionalMoney struct {
	Set   bool
	Value *Money
}

// OptionalString is OptionalTime for strings (a null clears).
type OptionalString struct {
	Set   bool
	Value *string
}

// TeamPatch is an UpdateTeam body. A nil pointer means the member was absent.
type TeamPatch struct {
	DisplayName              *string
	Description              *string
	OwnerEmail               *string
	CostCenter               *string
	IdentityProviderID       OptionalString
	Kind                     *TeamKind
	MaxBudget                *Money
	CeilingOverride          OptionalMoney
	CeilingOverrideExpiresAt OptionalTime
	CeilingOverrideReason    string
	Quotas                   *Quotas
	IsolationTier            *IsolationTier
	ViewerKubectl            *bool
	MayManageAwsAccounts     *bool
	MayAddExternalSystems    *bool
	MayExposeCollaboration   *bool
	AllowedAwsAccounts       *[]string
}

// GuardrailFields lists the guardrail fields the patch sets; they need teams:set-guardrails
// (API contract §4.2).
func (p TeamPatch) GuardrailFields() []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(p.MaxBudget != nil, "maxBudgetUsd")
	add(p.CeilingOverride.Set, "ceilingOverrideUsd")
	add(p.CeilingOverrideExpiresAt.Set, "ceilingOverrideExpiresAt")
	add(p.Quotas != nil, "quotas")
	add(p.IsolationTier != nil, "isolationTier")
	add(p.ViewerKubectl != nil, "viewerKubectl")
	add(p.MayManageAwsAccounts != nil, "mayManageAwsAccounts")
	add(p.MayAddExternalSystems != nil, "mayAddExternalSystems")
	add(p.MayExposeCollaboration != nil, "mayExposeCollaboration")
	return out
}

// SetsAllowedAccounts reports whether the patch changes allowedAwsAccounts (teams:set-aws-accounts).
func (p TeamPatch) SetsAllowedAccounts() bool { return p.AllowedAwsAccounts != nil }

// OnlyTeamWrite reports whether the patch sets nothing but teams:write fields.
func (p TeamPatch) OnlyTeamWrite() bool {
	return len(p.GuardrailFields()) == 0 && !p.SetsAllowedAccounts()
}

// SetsCeilingOverride reports whether the patch sets (not clears) an override.
func (p TeamPatch) SetsCeilingOverride() bool {
	return p.CeilingOverride.Set && p.CeilingOverride.Value != nil
}

// Apply returns t changed by p. budget is the team's current budget amount (nil when it has none):
// maxBudgetUsd may not go below it. A change to kind is a ValidationError. Generation is bumped when
// a desired-state field changes; Version is left to the database.
func (t Team) Apply(p TeamPatch, budget *Money, now time.Time) (Team, error) {
	var fe []FieldError
	if p.Kind != nil && *p.Kind != t.Kind {
		fe = append(fe, FieldError{Field: "kind", Reason: "cannot change after create"})
	}
	n := t
	if p.DisplayName != nil {
		n.DisplayName = *p.DisplayName
	}
	if p.Description != nil {
		n.Description = *p.Description
	}
	if p.OwnerEmail != nil {
		n.OwnerEmail = *p.OwnerEmail
	}
	if p.CostCenter != nil {
		n.CostCenter = *p.CostCenter
	}
	fe = append(fe, validateTeamText(n.DisplayName, n.Description, n.CostCenter, n.OwnerEmail)...)
	if p.IdentityProviderID.Set {
		n.IdentityProviderID = ""
		if p.IdentityProviderID.Value != nil {
			n.IdentityProviderID = *p.IdentityProviderID.Value
		}
	}
	if p.MaxBudget != nil {
		n.MaxBudget = *p.MaxBudget
		switch {
		case n.MaxBudget.IsNegative():
			fe = append(fe, FieldError{Field: "maxBudgetUsd", Reason: "must not be negative"})
		case budget != nil && n.MaxBudget.Cmp(*budget) < 0:
			fe = append(fe, FieldError{Field: "maxBudgetUsd", Reason: "is below the team's current budget amount " + budget.String()})
		}
	}
	fe = append(fe, t.applyOverride(&n, p, now)...)
	if p.Quotas != nil {
		n.Quotas = *p.Quotas
		if err := n.Quotas.Validate(); err != nil {
			fe = append(fe, FieldErrors(err)...)
		}
	}
	if p.IsolationTier != nil {
		n.IsolationTier = *p.IsolationTier
		if !n.IsolationTier.Valid() {
			fe = append(fe, FieldError{Field: "isolationTier", Reason: "must be shared or dedicated"})
		}
	}
	if p.ViewerKubectl != nil {
		n.ViewerKubectl = *p.ViewerKubectl
	}
	if p.MayManageAwsAccounts != nil {
		n.MayManageAwsAccounts = *p.MayManageAwsAccounts
	}
	if p.MayAddExternalSystems != nil {
		n.MayAddExternalSystems = *p.MayAddExternalSystems
	}
	if p.MayExposeCollaboration != nil {
		n.MayExposeCollaboration = *p.MayExposeCollaboration
	}
	if p.AllowedAwsAccounts != nil {
		n.AllowedAwsAccounts = slices.Clone(*p.AllowedAwsAccounts)
		if n.AllowedAwsAccounts == nil {
			n.AllowedAwsAccounts = []string{}
		}
		fe = append(fe, validateAccounts(n.AllowedAwsAccounts)...)
	}
	if len(fe) > 0 {
		return Team{}, Validation(fe...)
	}
	n.Generation++
	n.UpdatedAt = now.UTC()
	return n, nil
}

// applyOverride validates and applies the ceiling override members of p onto n.
func (t Team) applyOverride(n *Team, p TeamPatch, now time.Time) []FieldError {
	if !p.CeilingOverride.Set && !p.CeilingOverrideExpiresAt.Set {
		if n.CeilingOverride != nil && n.CeilingOverride.Cmp(n.MaxBudget) <= 0 {
			return []FieldError{{Field: "maxBudgetUsd", Reason: "must stay below the active ceiling override"}}
		}
		return nil
	}
	amount, expires := t.CeilingOverride, t.CeilingOverrideExpiresAt
	if p.CeilingOverride.Set {
		amount = p.CeilingOverride.Value
	}
	if p.CeilingOverrideExpiresAt.Set {
		expires = p.CeilingOverrideExpiresAt.Value
	}
	if amount == nil && expires == nil {
		n.CeilingOverride, n.CeilingOverrideExpiresAt = nil, nil
		return nil
	}
	var fe []FieldError
	if amount == nil || expires == nil {
		return []FieldError{{Field: "ceilingOverrideUsd", Reason: "the override needs both ceilingOverrideUsd and ceilingOverrideExpiresAt"}}
	}
	if strings.TrimSpace(p.CeilingOverrideReason) == "" {
		fe = append(fe, FieldError{Field: "ceilingOverrideReason", Reason: "is required with a ceiling override"})
	}
	if !expires.After(now) {
		fe = append(fe, FieldError{Field: "ceilingOverrideExpiresAt", Reason: "must be in the future"})
	}
	if amount.Cmp(n.MaxBudget) <= 0 {
		fe = append(fe, FieldError{Field: "ceilingOverrideUsd", Reason: "must be above maxBudgetUsd " + n.MaxBudget.String()})
	}
	if len(fe) == 0 {
		n.CeilingOverride, n.CeilingOverrideExpiresAt = amount, expires
	}
	return fe
}

// Suspend returns t suspended; only an active team can be suspended.
func (t Team) Suspend(now time.Time) (Team, error) {
	if t.State != TeamStateActive {
		return Team{}, invalidTeamState(t.State, "suspend")
	}
	t.State, t.Generation, t.UpdatedAt = TeamStateSuspended, t.Generation+1, now.UTC()
	return t, nil
}

// Resume returns t active again; only a suspended team can be resumed.
func (t Team) Resume(now time.Time) (Team, error) {
	if t.State != TeamStateSuspended {
		return Team{}, invalidTeamState(t.State, "resume")
	}
	t.State, t.Generation, t.UpdatedAt = TeamStateActive, t.Generation+1, now.UTC()
	return t, nil
}

// MarkDeleting returns t in state deleting (the reconciler finalizes it); an active or suspended
// team can be deleted. The status reads Deleting at once.
func (t Team) MarkDeleting(now time.Time) (Team, error) {
	if t.State != TeamStateActive && t.State != TeamStateSuspended {
		return Team{}, invalidTeamState(t.State, "delete")
	}
	t.State, t.Generation, t.UpdatedAt = TeamStateDeleting, t.Generation+1, now.UTC()
	t.Status.Phase = PhaseDeleting
	return t, nil
}

func invalidTeamState(s TeamState, action string) *Error {
	return Conflict("invalid_state", "cannot "+action+" a team in state "+string(s), map[string]any{"state": string(s)})
}

// WithDeletingStatus overrides the status phase with Deleting while the team is deleting, because the
// reconciler may not have written the status yet.
func (t Team) WithDeletingStatus() Team {
	if t.State == TeamStateDeleting && t.Status.Phase != PhaseDeleting {
		t.Status.Phase = PhaseDeleting
	}
	return t
}
