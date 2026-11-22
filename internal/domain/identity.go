package domain

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Role link limits (database 000006; API contract §5 RoleLink).
const (
	// DefaultRoleLinkSessionSeconds is the session length of a link that names none.
	DefaultRoleLinkSessionSeconds = 3600
	// MinRoleLinkSessionSeconds and MaxRoleLinkSessionSeconds bound maxSessionSeconds (15 minutes to 12 hours).
	MinRoleLinkSessionSeconds = 900
	MaxRoleLinkSessionSeconds = 43200
	maxRoleARNLen             = 600
	externalIDChars           = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	externalIDLen             = 32
)

var (
	roleARNRE     = regexp.MustCompile(`^arn:aws[a-z-]*:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]+$`)
	profileNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// ValidAWSAccountID reports whether s is a 12-digit AWS account id.
func ValidAWSAccountID(s string) bool { return awsAccountRE.MatchString(s) }

// AccountOfRoleARN returns the account id of an IAM role ARN, or "".
func AccountOfRoleARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || !ValidAWSAccountID(parts[4]) {
		return ""
	}
	return parts[4]
}

// RoleLink is a role in another account of the organization that a dedicated agent may assume
// (AGT-10). Its role ARN and external id never change; replace the link instead. The reconciler
// writes Status, LastVerifiedAt and LastError.
type RoleLink struct {
	ID                string
	TeamID            string
	AgentID           string
	AccountID         string
	RoleARN           string
	ProfileName       string
	ExternalID        string
	MaxSessionSeconds int
	Status            RoleLinkStatus
	LastVerifiedAt    *time.Time
	LastError         string
	Version           Version
	CreatedAt         time.Time
	CreatedBy         string
	UpdatedAt         time.Time
}

// NewRoleLinkParams is the request of CreateAgentRoleLink.
type NewRoleLinkParams struct {
	RoleARN           string
	ProfileName       string
	MaxSessionSeconds int
}

// Validate checks the request shape. The account filter and the agent's identity are checked against
// the database by the use case.
func (p NewRoleLinkParams) Validate() error {
	var fe []FieldError
	switch {
	case p.RoleARN == "":
		fe = append(fe, FieldError{Field: "roleArn", Reason: "is required"})
	case len(p.RoleARN) > maxRoleARNLen || !roleARNRE.MatchString(p.RoleARN):
		fe = append(fe, FieldError{Field: "roleArn", Reason: "must be an IAM role ARN such as arn:aws:iam::111122223333:role/deployer"})
	}
	switch {
	case p.ProfileName == "":
		fe = append(fe, FieldError{Field: "profileName", Reason: "is required"})
	case !profileNameRE.MatchString(p.ProfileName):
		fe = append(fe, FieldError{Field: "profileName", Reason: "must start with a lowercase letter and hold lowercase letters, digits and hyphens, at most 63 characters"})
	}
	if p.MaxSessionSeconds != 0 && (p.MaxSessionSeconds < MinRoleLinkSessionSeconds || p.MaxSessionSeconds > MaxRoleLinkSessionSeconds) {
		fe = append(fe, FieldError{Field: "maxSessionSeconds", Reason: "must be between 900 and 43200"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// NewExternalID generates an external id for a link from r: "afs-" and 32 letters and digits (database
// 000006). It is a confused-deputy guard shown to the target account's owner, not a secret.
func NewExternalID(r io.Reader) (string, error) {
	b := make([]byte, externalIDLen)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("domain: external id: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("afs-")
	for _, x := range b {
		// 62 does not divide 256; the bias is irrelevant for a guard of 190 bits.
		sb.WriteByte(externalIDChars[int(x)%len(externalIDChars)])
	}
	return sb.String(), nil
}

// NewRoleLink builds a pending_trust link for an agent. The caller gives the id and the external id.
func NewRoleLink(id, teamID, agentID, externalID string, p NewRoleLinkParams, now time.Time) (RoleLink, error) {
	if err := p.Validate(); err != nil {
		return RoleLink{}, err
	}
	max := p.MaxSessionSeconds
	if max == 0 {
		max = DefaultRoleLinkSessionSeconds
	}
	return RoleLink{ID: id, TeamID: teamID, AgentID: agentID, AccountID: AccountOfRoleARN(p.RoleARN), RoleARN: p.RoleARN,
		ProfileName: p.ProfileName, ExternalID: externalID, MaxSessionSeconds: max, Status: RoleLinkStatusPendingTrust,
		Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// TrustPolicy is the IAM trust policy the target account's owner puts on the linked role (AGT-10): the
// principal is the agent's own role, the conditions are the link's external id and the role tag
// afs-team-id (a property of the role, so it holds through a role chain, D26). The ARN of the agent's
// role may be unknown (empty) before the agent's role exists; the policy is then omitted.
func (l RoleLink) TrustPolicy(agentRoleARN string) map[string]any {
	if agentRoleARN == "" {
		return nil
	}
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": agentRoleARN},
			"Action":    []any{"sts:AssumeRole", "sts:TagSession"},
			"Condition": map[string]any{"StringEquals": map[string]any{
				"sts:ExternalId":               l.ExternalID,
				"aws:PrincipalTag/afs-team-id": l.TeamID,
			}},
		}},
	}
}

// Audit is the audit view of the link: nothing secret is in it, the external id is a guard.
func (l RoleLink) Audit() map[string]any {
	return map[string]any{"id": l.ID, "agentId": l.AgentID, "accountId": l.AccountID, "roleArn": l.RoleARN,
		"profileName": l.ProfileName, "maxSessionSeconds": l.MaxSessionSeconds, "status": string(l.Status)}
}

// AgentIdentityKind is the kind of an agent identity (API contract §5).
type AgentIdentityKind string

// AgentIdentityKind values. Only github_app and external are created through the API; the other two
// are managed by AFS.
const (
	IdentityK8sServiceAccount AgentIdentityKind = "k8s_service_account"
	IdentityAWSIAMRole        AgentIdentityKind = "aws_iam_role"
	IdentityGitHubApp         AgentIdentityKind = "github_app"
	IdentityExternal          AgentIdentityKind = "external"
)

// Valid reports whether k is a known kind.
func (k AgentIdentityKind) Valid() bool {
	switch k {
	case IdentityK8sServiceAccount, IdentityAWSIAMRole, IdentityGitHubApp, IdentityExternal:
		return true
	}
	return false
}

// Creatable reports whether a person may create an identity of the kind.
func (k AgentIdentityKind) Creatable() bool { return k == IdentityGitHubApp || k == IdentityExternal }

// AgentIdentityStatus is the reconciliation state of an identity.
type AgentIdentityStatus string

// AgentIdentityStatus values.
const (
	IdentityPending  AgentIdentityStatus = "pending"
	IdentityActive   AgentIdentityStatus = "active"
	IdentityDisabled AgentIdentityStatus = "disabled"
	IdentityError    AgentIdentityStatus = "error"
)

// AgentIdentity is a credential-bearing identity of an agent: its ServiceAccount, its IAM role, a
// GitHub App installation or an external account. Credentials are secret references, never values.
type AgentIdentity struct {
	ID          string
	TeamID      string
	AgentID     string
	Kind        AgentIdentityKind
	ExternalRef string
	SecretID    string
	Status      AgentIdentityStatus
	Version     Version
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
}

// NewAgentIdentityParams is the request of CreateAgentIdentity.
type NewAgentIdentityParams struct {
	Kind        AgentIdentityKind
	ExternalRef string
	SecretID    string
}

const maxIdentityRefLen = 512

// Validate checks the request shape. That the secret belongs to the team is checked by the use case.
func (p NewAgentIdentityParams) Validate() error {
	var fe []FieldError
	switch {
	case p.Kind == "":
		fe = append(fe, FieldError{Field: "kind", Reason: "is required"})
	case !p.Kind.Creatable():
		fe = append(fe, FieldError{Field: "kind", Reason: "must be github_app or external; the other kinds are managed by AFS"})
	}
	switch {
	case strings.TrimSpace(p.ExternalRef) == "":
		fe = append(fe, FieldError{Field: "externalRef", Reason: "is required"})
	case len(p.ExternalRef) > maxIdentityRefLen:
		fe = append(fe, FieldError{Field: "externalRef", Reason: "must be at most 512 characters"})
	case strings.ContainsAny(p.ExternalRef, "\r\n\t"):
		fe = append(fe, FieldError{Field: "externalRef", Reason: "must be one line of text"})
	}
	if p.SecretID != "" {
		if _, err := ParseID(PrefixSecret, p.SecretID); err != nil {
			fe = append(fe, FieldError{Field: "secretId", Reason: "must be a sec_ id"})
		}
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// NewAgentIdentity builds a pending identity for an agent.
func NewAgentIdentity(id, teamID, agentID string, p NewAgentIdentityParams, now time.Time) (AgentIdentity, error) {
	if err := p.Validate(); err != nil {
		return AgentIdentity{}, err
	}
	return AgentIdentity{ID: id, TeamID: teamID, AgentID: agentID, Kind: p.Kind, ExternalRef: strings.TrimSpace(p.ExternalRef),
		SecretID: p.SecretID, Status: IdentityPending, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// Audit is the audit view of the identity (the secret reference, never a value).
func (i AgentIdentity) Audit() map[string]any {
	return map[string]any{"id": i.ID, "agentId": i.AgentID, "kind": string(i.Kind), "externalRef": i.ExternalRef,
		"secretId": i.SecretID, "status": string(i.Status)}
}

// OrgAccountStatus is the status of an Organizations account.
type OrgAccountStatus string

// OrgAccountStatus values.
const (
	OrgAccountActive         OrgAccountStatus = "active"
	OrgAccountSuspended      OrgAccountStatus = "suspended"
	OrgAccountPendingClosure OrgAccountStatus = "pending_closure"
)

// OrgAccount is one account of the AWS organization, mirrored hourly (TEAM-8, RC-8).
type OrgAccount struct {
	ID       string
	Name     string
	Status   OrgAccountStatus
	SyncedAt time.Time
}

// GlobMatch reports whether s matches the glob pattern, where * matches any run of characters and ?
// one character. It mirrors afs.glob_like.
func GlobMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	var pi, ti int
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) && p[pi] != '*':
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// AccountAllowed reports whether the account is active and matches one of the team's allowed-accounts
// patterns by id or name (TEAM-8). It mirrors afs.account_allowed.
func AccountAllowed(patterns []string, a OrgAccount) bool {
	if a.Status != OrgAccountActive {
		return false
	}
	for _, p := range patterns {
		if GlobMatch(p, a.ID) || GlobMatch(p, a.Name) {
			return true
		}
	}
	return false
}
