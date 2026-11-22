package domain

import (
	"regexp"
	"strings"
)

var (
	nameRE       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)
	configNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
	digestRE     = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// Name is a team slug, agent, pool or secret name (API contract §1). Names are immutable after create.
type Name string

// ParseName validates s as a Name; field names the request field for the error.
func ParseName(field, s string) (Name, error) {
	if !nameRE.MatchString(s) {
		return "", Validation(FieldError{Field: field, Reason: "must match ^[a-z][a-z0-9-]{1,38}[a-z0-9]$"})
	}
	return Name(s), nil
}

// ConfigName is the name of a configuration value: UPPER_SNAKE, and never credential-like.
type ConfigName string

// credentialWords are name fragments the database also refuses, checked here for a clear error.
var credentialWords = []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "PRIVATE_KEY", "API_KEY", "ACCESS_KEY", "CREDENTIAL"}

// ParseConfigName validates s as a ConfigName.
func ParseConfigName(field, s string) (ConfigName, error) {
	if !configNameRE.MatchString(s) {
		return "", Validation(FieldError{Field: field, Reason: "must be UPPER_SNAKE (^[A-Z][A-Z0-9_]*$)"})
	}
	for _, w := range credentialWords {
		if strings.Contains(s, w) {
			return "", Validation(FieldError{Field: field, Reason: "looks like a credential; store it as a secret"})
		}
	}
	return ConfigName(s), nil
}

// ImageRef is a container image reference pinned by digest (repo@sha256:<64 hex>).
type ImageRef string

// ParseImageRef validates s as a digest-pinned image reference.
func ParseImageRef(field, s string) (ImageRef, error) {
	if !digestRE.MatchString(s) || strings.HasPrefix(s, "@") {
		return "", Validation(FieldError{Field: field, Reason: "must be pinned by digest (@sha256:<64 hex>)"})
	}
	return ImageRef(s), nil
}
