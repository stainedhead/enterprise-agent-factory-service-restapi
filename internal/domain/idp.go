package domain

import (
	"regexp"
	"time"
)

var (
	issuerRE         = regexp.MustCompile(`^https://[^\s/]+(/[^\s]*)?$`)
	groupsClaimRE    = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,100}$`)
	collabClientIDRE = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,255}$`)
	secretARNRE      = regexp.MustCompile(`^arn:aws[a-z-]*:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:.+$`)
)

// IdentityProvider is a service-level OIDC provider (API contract §5). It never holds a client
// secret, only the Secrets Manager ARN where one lives.
type IdentityProvider struct {
	ID              string
	Kind            IdPKind
	Name            string
	IssuerURL       string
	ConsoleClientID string
	CLIClientID     string
	ClientSecretARN string
	// CollabClientID and CollabClientSecretARN are the OIDC client the collaboration edge signs people in
	// with; the ARN is a reference, never a value. Both empty when the provider has no collaboration client.
	CollabClientID        string
	CollabClientSecretARN string
	GroupsClaim           string
	IsDefault             bool
	Enabled               bool
	Version               Version
	CreatedAt             time.Time
	CreatedBy             string
	UpdatedAt             time.Time
}

// Validate checks what the database also checks, for clear errors: https issuer, ids, ARN shape,
// and that the default provider is enabled.
func (p IdentityProvider) Validate() error {
	var fe []FieldError
	bad := func(f, r string) { fe = append(fe, FieldError{Field: f, Reason: r}) }
	if !p.Kind.Valid() {
		bad("kind", "must be okta or entra_id")
	}
	if n := len([]rune(p.Name)); n < 1 || n > 100 {
		bad("name", "must be 1 to 100 characters")
	}
	if !issuerRE.MatchString(p.IssuerURL) || len(p.IssuerURL) > 500 {
		bad("issuerUrl", "must be an https URL")
	}
	if n := len(p.ConsoleClientID); n < 1 || n > 200 {
		bad("consoleClientId", "is required (1 to 200 characters)")
	}
	if len(p.CLIClientID) > 200 {
		bad("cliClientId", "must be at most 200 characters")
	}
	if p.ClientSecretARN != "" && !secretARNRE.MatchString(p.ClientSecretARN) {
		bad("clientSecretArn", "must be a Secrets Manager secret ARN")
	}
	if p.CollabClientID != "" && !collabClientIDRE.MatchString(p.CollabClientID) {
		bad("collabClientId", "must match ^[A-Za-z0-9._:@-]{1,255}$")
	}
	if p.CollabClientSecretARN != "" {
		if !secretARNRE.MatchString(p.CollabClientSecretARN) {
			bad("collabClientSecretArn", "must be a Secrets Manager secret ARN")
		}
		if p.CollabClientID == "" {
			bad("collabClientSecretArn", "needs collabClientId")
		}
	}
	if !groupsClaimRE.MatchString(p.GroupsClaim) {
		bad("groupsClaim", "must be a claim name of 1 to 100 characters")
	}
	if p.IsDefault && !p.Enabled {
		bad("isDefault", "the default provider must be enabled")
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}
