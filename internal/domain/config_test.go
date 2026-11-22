package domain

import (
	"strings"
	"testing"
)

func TestValidateConfigValue(t *testing.T) {
	tests := []struct {
		name  string
		cname ConfigName
		value string
		ok    bool
	}{
		{"plain", "LOG_LEVEL", "debug", true},
		{"empty", "LOG_LEVEL", "", true},
		{"at limit", "BIG", strings.Repeat("x", 4096), true},
		{"over limit", "BIG", strings.Repeat("x", 4097), false},
		{"NUL", "X", "a\x00b", false},
		{"owned repos ok", OwnedReposName, "github.com/acme/payments-api, github.com/acme/*\ngithub.com/Acme/Web", true},
		{"owned repos empty", OwnedReposName, "", true},
		{"owned repos two parts", OwnedReposName, "acme/payments", false},
		{"owned repos four parts", OwnedReposName, "github.com/acme/a/b", false},
		{"owned repos wildcard host", OwnedReposName, "*/acme/x", false},
		{"owned repos wildcard owner", OwnedReposName, "github.com/*/x", false},
		{"owned repos partial wildcard", OwnedReposName, "github.com/acme/pay*", false},
		{"owned repos bad char", OwnedReposName, "github.com/acme/a b c", false},
		{"owned repos long entry", OwnedReposName, "github.com/acme/" + strings.Repeat("r", 101), false},
		{"other name not parsed", "SOMETHING", "not a repo", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfigValue("value", tc.cname, tc.value)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v", err)
			}
			if err != nil && KindOf(err) != KindValidation {
				t.Errorf("kind = %v", KindOf(err))
			}
		})
	}
	err := ValidateOwnedRepos("value", "github.com/acme/"+strings.Repeat("r", 101)+"/")
	if err == nil || !strings.Contains(err.Error(), "...") {
		t.Errorf("long entries are shortened in the message: %v", err)
	}
}
