package mcp

// CredentialKind is how the gateway authenticates to an upstream server (PRD §9).
type CredentialKind string

// Credential kinds.
const (
	CredNone               CredentialKind = "none"
	CredStatic             CredentialKind = "static"              // header from a Secrets Manager reference
	CredOAuthClient        CredentialKind = "oauth_client"        //nolint:gosec // a kind name, not a credential
	CredWorkloadFederation CredentialKind = "workload_federation" // RFC 8693 exchange, nothing stored
	CredOAuthUser          CredentialKind = "oauth_user"          //nolint:gosec // a kind name, not a credential
	CredAWSSigV4           CredentialKind = "aws_sigv4"           // signed with the team gateway role
)

// Valid reports whether k is a known kind.
func (k CredentialKind) Valid() bool {
	switch k {
	case CredNone, CredStatic, CredOAuthClient, CredWorkloadFederation, CredOAuthUser, CredAWSSigV4:
		return true
	}
	return false
}

// StoresSecret reports whether the kind needs a Secrets Manager reference. Workload federation and
// SigV4 store nothing: the gateway exchanges or signs per call.
func (k CredentialKind) StoresSecret() bool {
	return k == CredStatic || k == CredOAuthClient || k == CredOAuthUser
}
