package domain

import (
	"strings"
	"time"
)

// maxConfigValueLen is the database limit of a configuration value (afs.namespace_config_values).
const maxConfigValueLen = 4096

// OwnedReposName is the namespace configuration value that lists the repositories a team owns
// (WO-7): `host/owner/repo` or `host/owner/*`, comma or whitespace separated, case-insensitive.
const OwnedReposName ConfigName = "AFS_OWNED_REPOS"

// NamespaceConfigValue is a non-secret value set for a team's namespace, shared by its agents.
type NamespaceConfigValue struct {
	ID        string
	TeamID    string
	Name      ConfigName
	Value     string
	Version   Version
	CreatedAt time.Time
	UpdatedAt time.Time
	// UsedBy lists the agents that link to the value (read only).
	UsedBy []string
}

// AgentConfigValue is one entry of an agent's configuration: its own literal value or a link to the
// namespace value of the same name. ResolvedValue is the effective value.
type AgentConfigValue struct {
	ID            string
	TeamID        string
	AgentID       string
	Name          ConfigName
	Source        ConfigSource
	Value         *string
	ResolvedValue *string
	Version       Version
	UpdatedAt     time.Time
}

// ValidateConfigValue checks the text of a configuration value for name: at most 4096 characters, no
// NUL, and for AFS_OWNED_REPOS a list of valid repository entries.
func ValidateConfigValue(field string, name ConfigName, value string) error {
	if len(value) > maxConfigValueLen {
		return Validation(FieldError{Field: field, Reason: "must be at most 4096 characters"})
	}
	if strings.ContainsRune(value, 0) {
		return Validation(FieldError{Field: field, Reason: "must not contain NUL"})
	}
	if name == OwnedReposName {
		return ValidateOwnedRepos(field, value)
	}
	return nil
}

// ValidateOwnedRepos checks an AFS_OWNED_REPOS value: entries separated by commas or whitespace,
// each `host/owner/repo` or `host/owner/*`. An empty value lists nothing and is valid.
func ValidateOwnedRepos(field, value string) error {
	for _, e := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		parts := strings.Split(e, "/")
		ok := len(parts) == 3
		if ok {
			host, owner, repo := parts[0], parts[1], parts[2]
			ok = endpointHost.MatchString(strings.ToLower(host)) && repoSegmentOK(owner) && (repo == "*" || repoSegmentOK(repo))
		}
		if !ok {
			return Validation(FieldError{Field: field, Reason: "entry " + quoteEntry(e) + " must be host/owner/repo or host/owner/*"})
		}
	}
	return nil
}

func repoSegmentOK(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func quoteEntry(e string) string {
	if len(e) > 60 {
		e = e[:60] + "..."
	}
	return `"` + e + `"`
}
