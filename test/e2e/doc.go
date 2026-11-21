// Package e2e holds end-to-end suites that run against the dev cluster and AWS account. Every test
// file carries the e2e build tag; CI compiles them (go vet -tags e2e) but never runs them.
package e2e
