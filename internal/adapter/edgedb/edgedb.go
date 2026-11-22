// Package edgedb is the database adapter of afs-edge. The afs_edge role has no table privilege: every call
// here is one of the ten SECURITY DEFINER functions of -db migration 000020, so the edge can read routes and
// bindings, store sessions and add access counts, and nothing else.
package edgedb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// Querier is the pgx surface the adapter needs; *pgxpool.Pool satisfies it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB implements the database ports of the edge use cases.
type DB struct{ q Querier }

// New returns the adapter on a pool connected as afs_edge.
func New(q Querier) *DB { return &DB{q: q} }

var (
	_ edge.RouteSource = (*DB)(nil)
	_ edge.Principals  = (*DB)(nil)
	_ edge.Sessions    = (*DB)(nil)
	_ edge.AccessSink  = (*DB)(nil)
	_ edge.Alerts      = (*DB)(nil)
)

func mapErr(err error, what string) error { return postgres.MapError(err, postgres.OpRead, what) }

// Settings implements edge.RouteSource: the collab.* service settings, defaults for what is not set.
func (d *DB) Settings(ctx context.Context) (collab.Settings, error) {
	rows, err := d.q.Query(ctx, `SELECT key, value FROM afs.collab_settings()`)
	if err != nil {
		return collab.Settings{}, mapErr(err, "collab settings")
	}
	defer rows.Close()
	s := collab.DefaultSettings()
	for rows.Next() {
		var k string
		var raw []byte
		if err := rows.Scan(&k, &raw); err != nil {
			return s, mapErr(err, "collab settings")
		}
		applySetting(&s, k, raw)
	}
	return s, mapErr(rows.Err(), "collab settings")
}

// applySetting stores one setting; a value of the wrong type keeps the default (the API validates on write).
func applySetting(s *collab.Settings, key string, raw []byte) {
	switch key {
	case "collab.base_domain":
		_ = json.Unmarshal(raw, &s.BaseDomain)
	case "collab.public_endpoints":
		_ = json.Unmarshal(raw, &s.PublicEndpoints)
	case "collab.allowed_source_cidrs":
		_ = json.Unmarshal(raw, &s.AllowedSourceCIDRs)
	case "collab.session_ttl_minutes":
		_ = json.Unmarshal(raw, &s.SessionTTLMinutes)
	case "collab.content_scan":
		var v string
		if json.Unmarshal(raw, &v) == nil && (v == "block" || v == "warn" || v == "off") {
			s.ContentScan = v
		}
	case "collab.max_body_mb":
		var v int
		if json.Unmarshal(raw, &v) == nil && v > 0 {
			s.MaxBodyMB = v
		}
	case "collab.denied_spike_threshold":
		_ = json.Unmarshal(raw, &s.DeniedSpikeThreshold)
	}
}

// Routes implements edge.RouteSource. Teams without a hostname (no base domain) have no route.
func (d *DB) Routes(ctx context.Context) ([]collab.Route, error) {
	rows, err := d.q.Query(ctx, `SELECT team_id, slug, namespace, hostname, team_state, agent_id, agent_state,
		web_ui_enabled, rest_api_enabled, web_ui_effective, rest_api_effective, may_expose, ready, not_ready_reason,
		service_name, port, provider_id, issuer_url, groups_claim, collab_client_id, collab_client_secret_arn
		FROM afs.collab_routes()`)
	if err != nil {
		return nil, mapErr(err, "collab routes")
	}
	defer rows.Close()
	var out []collab.Route
	for rows.Next() {
		var r collab.Route
		var host, agentID, agentState, reason, prov, issuer, groups, client, arn *string
		if err := rows.Scan(&r.TeamID, &r.Slug, &r.Namespace, &host, &r.TeamState, &agentID, &agentState,
			&r.WebUIEnabled, &r.RESTAPIEnabled, &r.WebUIEffective, &r.RESTAPIEffective, &r.MayExpose, &r.Ready, &reason,
			&r.ServiceName, &r.Port, &prov, &issuer, &groups, &client, &arn); err != nil {
			return nil, mapErr(err, "collab routes")
		}
		if host == nil {
			continue
		}
		r.Hostname = *host
		r.AgentID, r.AgentState, r.NotReadyReason = deref(agentID), deref(agentState), deref(reason)
		r.ProviderID, r.IssuerURL, r.GroupsClaim = deref(prov), deref(issuer), deref(groups)
		r.ClientID, r.ClientSecretARN = deref(client), deref(arn)
		out = append(out, r)
	}
	return out, mapErr(rows.Err(), "collab routes")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Person implements edge.Principals.
func (d *DB) Person(ctx context.Context, teamID, subject, providerID string, groups []string) ([]edge.Binding, error) {
	if groups == nil {
		groups = []string{}
	}
	var prov any
	if providerID != "" {
		prov = providerID
	}
	rows, err := d.q.Query(ctx, `SELECT role, subject_type FROM afs.collab_principal($1, $2, $3, $4)`, teamID, subject, prov, groups)
	if err != nil {
		return nil, mapErr(err, "collab principal")
	}
	defer rows.Close()
	var out []edge.Binding
	for rows.Next() {
		var b edge.Binding
		var subjectType string
		if err := rows.Scan(&b.Role, &subjectType); err != nil {
			return nil, mapErr(err, "collab principal")
		}
		b.Group = subjectType == "group"
		out = append(out, b)
	}
	return out, mapErr(rows.Err(), "collab principal")
}

// Key implements edge.Principals.
func (d *DB) Key(ctx context.Context, teamID string, sha [32]byte) (edge.KeyInfo, bool, error) {
	var k edge.KeyInfo
	var exp *time.Time
	var revoked *time.Time
	err := d.q.QueryRow(ctx, `SELECT key_id, role, state, expires_at, revoked_at FROM afs.collab_principal_key($1, $2)`, teamID, sha[:]).
		Scan(&k.ID, &k.Role, &k.State, &exp, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return edge.KeyInfo{}, false, nil
	}
	if err != nil {
		return edge.KeyInfo{}, false, mapErr(err, "collab key")
	}
	if exp != nil {
		k.ExpiresAt = *exp
	}
	return k, true, nil
}

// HandoffCreate implements edge.Sessions.
func (d *DB) HandoffCreate(ctx context.Context, codeHash [32]byte, teamID, host, issuer, subject, providerID string, groups []string) error {
	if groups == nil {
		groups = []string{}
	}
	var prov any
	if providerID != "" {
		prov = providerID
	}
	var exp time.Time
	err := d.q.QueryRow(ctx, `SELECT afs.collab_handoff_create($1, $2, $3, $4, $5, $6, $7)`,
		codeHash[:], teamID, host, issuer, subject, prov, groups).Scan(&exp)
	return mapErr(err, "collab handoff")
}

// HandoffConsume implements edge.Sessions.
func (d *DB) HandoffConsume(ctx context.Context, codeHash [32]byte, host string, sessionHash [32]byte) (string, time.Time, bool, error) {
	var team string
	var exp time.Time
	err := d.q.QueryRow(ctx, `SELECT team_id, expires_at FROM afs.collab_handoff_consume($1, $2, $3)`, codeHash[:], host, sessionHash[:]).Scan(&team, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, mapErr(err, "collab handoff")
	}
	return team, exp, true, nil
}

// Get implements edge.Sessions.
func (d *DB) Get(ctx context.Context, idHash [32]byte) (edge.Session, bool, error) {
	var s edge.Session
	var prov *string
	var seen *time.Time
	err := d.q.QueryRow(ctx, `SELECT team_id, issuer, subject, provider_id, groups, expires_at, last_seen_at FROM afs.collab_session_get($1)`, idHash[:]).
		Scan(&s.TeamID, &s.Issuer, &s.Subject, &prov, &s.Groups, &s.ExpiresAt, &seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return edge.Session{}, false, nil
	}
	if err != nil {
		return edge.Session{}, false, mapErr(err, "collab session")
	}
	s.ProviderID = deref(prov)
	return s, true, nil
}

// Revoke implements edge.Sessions.
func (d *DB) Revoke(ctx context.Context, idHash [32]byte) error {
	var ok bool
	return mapErr(d.q.QueryRow(ctx, `SELECT afs.collab_session_revoke($1)`, idHash[:]).Scan(&ok), "collab session")
}

type accessRow struct {
	BucketStart       string `json:"bucket_start"`
	TeamID            string `json:"team_id"`
	Surface           string `json:"surface"`
	PrincipalKind     string `json:"principal_kind"`
	PrincipalID       string `json:"principal_id"`
	Outcome           string `json:"outcome"`
	ServiceAdminEntry bool   `json:"service_admin_entry"`
	Requests          int    `json:"requests"`
	Bytes             int64  `json:"bytes"`
}

// Record implements edge.AccessSink: one call, at most 1000 rows. The edge does not retry a batch that
// succeeded because the counts are additive.
func (d *DB) Record(ctx context.Context, rows []collab.Row) error {
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > collab.MaxFlushRows {
		return fmt.Errorf("edgedb: %d rows exceed one call", len(rows))
	}
	out := make([]accessRow, len(rows))
	for i, r := range rows {
		out[i] = accessRow{BucketStart: r.Key.Bucket.UTC().Format(time.RFC3339), TeamID: r.Key.TeamID, Surface: string(r.Key.Surface),
			PrincipalKind: string(r.Key.Kind), PrincipalID: r.Key.PrincipalID, Outcome: r.Key.Outcome,
			ServiceAdminEntry: r.Key.ServiceAdminEntry, Requests: r.Requests, Bytes: r.Bytes}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	var n int
	return mapErr(d.q.QueryRow(ctx, `SELECT afs.record_collab_access($1::jsonb)`, string(b)).Scan(&n), "collab access")
}

// SecretDetected implements edge.Alerts. Only the pattern type and a count are sent, never the match.
func (d *DB) SecretDetected(ctx context.Context, teamID string, surface collab.Surface, kind collab.PrincipalKind, principalID, pattern, action string, count int) error {
	var id int64
	return mapErr(d.q.QueryRow(ctx, `SELECT afs.emit_collab_secret_detected($1, $2, $3, $4, $5, $6, $7)`,
		teamID, string(surface), string(kind), principalID, pattern, action, count).Scan(&id), "collab secret detected")
}
