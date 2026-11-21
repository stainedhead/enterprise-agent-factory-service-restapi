// Package matrix is the authorization matrix harness (M1-D6, RA-US-1.3, CT-API-8). For every
// implemented operation and every kind of caller it sends a real request through the assembled
// afs-api handler against a real database and checks the answer against an oracle computed from the
// contract's own permission table (api/contract/permissions.json, operations.json) and the PRD §3
// rule: 401 without a credential, 404 for a caller not bound to the team, 403 with the required
// permission for a caller who is bound or on a service route, 2xx otherwise. It also checks the
// audit and outbox rows each mutation must write and the single denied audit row of every 403.
//
// It is a library rather than a *_test.go file because the handler is assembled in cmd/afs-api
// (package main), which cannot be imported: cmd/afs-api's matrix_test.go calls Run. A stream adds
// the seed data its operations need in fixtures_<area>.go in this directory (an init that calls
// Register); a test fails for an implemented operation that has no case and no exemption.
package matrix
