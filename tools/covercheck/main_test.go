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
