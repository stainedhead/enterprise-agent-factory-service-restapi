package main

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	in := strings.Join([]string{
		"ok  \tx/internal/domain\t0.01s\tcoverage: 99.5% of statements",
		"ok  \tx/internal/authz\t0.01s\tcoverage: 80.0% of statements",
		"\tx/internal/usecase\t\tcoverage: 0.0% of statements",
		"?   \tx/internal/usecase/port\t[no test files]",
	}, "\n")
	low := check(strings.NewReader(in), 90)
	if len(low) != 2 || !strings.Contains(low[0], "authz") || !strings.Contains(low[1], "usecase") {
		t.Fatalf("low = %v", low)
	}
}

// Packages of in-memory fakes (teamstest, memberstest, ...) are test support: their import path ends in
// "test", and what they do is proven by the tests of the packages that use them.
func TestCheckSkipsFakePackages(t *testing.T) {
	in := strings.Join([]string{
		"ok  \tx/internal/usecase/teams/teamstest\t0.01s\tcoverage: 12.0% of statements",
		"ok  \tx/internal/usecase/reconcile/team/teamtest\t0.01s\tcoverage: 0.0% of statements",
		"ok  \tx/internal/usecase/latest\t0.01s\tcoverage: 10.0% of statements",
	}, "\n")
	low := check(strings.NewReader(in), 90)
	if len(low) != 1 || !strings.Contains(low[0], "usecase/latest") {
		t.Fatalf("low = %v (a package named latest is not a fake)", low)
	}
}
