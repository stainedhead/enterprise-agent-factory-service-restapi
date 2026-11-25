package postgres

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// teamServerSelect is mcpServerSelect plus the team's switch, the credential the gateway would use for the
// team and the team's federation state. $1 is the team.
var teamServerSelect = strings.Replace(mcpServerSelect, "\nFROM afs.mcp_servers s", `,
  sw.enabled, sw.mode, sw.enabled_by, sw.enabled_at, sw.version, sw.updated_at,
  c.id, c.scope, c.kind, c.secret_arn, c.subject, c.scopes, c.expires_at, c.status, c.rotated_at, c.version,
  coalesce(fs.status, '')
FROM afs.mcp_servers s`, 1) + `
LEFT JOIN afs.team_mcp_servers sw ON sw.team_id = $1 AND sw.server_id = s.id
LEFT JOIN afs.mcp_credentials c ON c.server_id = s.id AND c.scope = CASE WHEN s.team_id IS NOT NULL THEN s.team_id
  WHEN s.credential_mode = 'shared' THEN '_shared' ELSE $1 END
LEFT JOIN afs.mcp_federation_states fs ON fs.server_id = s.id AND fs.team_id = $1
WHERE (s.team_id = $1 OR (s.team_id IS NULL AND (s.published OR sw.team_id IS NOT NULL)))`

func scanTeamServer(row pgx.Row, teamID string) (domain.TeamMcpServerRow, error) {
	var (
		swEnabled                                 *bool
		swMode, swBy                              *string
		swAt, swUpdated                           *time.Time
		swVersion, cVersion                       *int64
		cID, cScope, cKind, cARN, cSubject, cStat *string
		cScopes                                   []string
		cExpires, cRotated                        *time.Time
		r                                         domain.TeamMcpServerRow
	)
	srv, err := scanMcpServer(row, &swEnabled, &swMode, &swBy, &swAt, &swVersion, &swUpdated,
		&cID, &cScope, &cKind, &cARN, &cSubject, &cScopes, &cExpires, &cStat, &cRotated, &cVersion, &r.FederationState)
	if err != nil {
		return r, err
	}
	r.Server = srv
	if swEnabled != nil {
		r.Switch = &domain.TeamMcpSwitch{TeamID: teamID, ServerID: srv.ID, Enabled: *swEnabled, Mode: domain.McpTeamMode(deref(swMode)),
			EnabledBy: deref(swBy), EnabledAt: *swAt, Version: domain.Version(*swVersion), UpdatedAt: *swUpdated}
	}
	if cID != nil {
		r.Credential = &domain.McpCredentialRecord{ID: *cID, ServerID: srv.ID, Scope: deref(cScope), Kind: deref(cKind), SecretARN: deref(cARN),
			Subject: deref(cSubject), Scopes: cScopes, ExpiresAt: cExpires, Status: deref(cStat), RotatedAt: cRotated,
			Version: domain.Version(*cVersion)}
		if r.Credential.Scope != domain.McpSharedScope {
			r.Credential.TeamID = r.Credential.Scope
		}
	}
	return r, nil
}

// TeamInfo implements mcpcatalog.Repository.
func (*McpRepo) TeamInfo(ctx context.Context, tx port.Tx, teamID string) (mcpcatalog.TeamInfo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return mcpcatalog.TeamInfo{}, err
	}
	var t mcpcatalog.TeamInfo
	err = pt.QueryRow(ctx, `SELECT id, slug, namespace, may_add_mcp_servers, gateway_deployment FROM afs.teams
WHERE id = $1 AND deleted_at IS NULL`, teamID).Scan(&t.ID, &t.Slug, &t.Namespace, &t.MayAddMcpServers, &t.GatewayDeployment)
	return t, MapError(err, OpRead, "team")
}

// ListTeamServers implements mcpcatalog.Repository.
func (*McpRepo) ListTeamServers(ctx context.Context, tx port.Tx, teamID string) ([]domain.TeamMcpServerRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []domain.TeamMcpServerRow{}
	err = collect(ctx, pt, "MCP server", teamServerSelect+` ORDER BY s.id`, []any{teamID}, func(rows pgx.Rows) error {
		r, err := scanTeamServer(rows, teamID)
		out = append(out, r)
		return err
	})
	return out, err
}

// GetTeamServer implements mcpcatalog.Repository.
func (*McpRepo) GetTeamServer(ctx context.Context, tx port.Tx, teamID, serverID string) (domain.TeamMcpServerRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.TeamMcpServerRow{}, err
	}
	r, err := scanTeamServer(pt.QueryRow(ctx, teamServerSelect+` AND s.id = $2`, teamID, serverID), teamID)
	return r, MapError(err, OpRead, "MCP server")
}

// PutSwitch implements mcpcatalog.Repository.
func (*McpRepo) PutSwitch(ctx context.Context, tx port.Tx, sw domain.TeamMcpSwitch) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	mode := sw.Mode
	if mode == "" {
		mode = domain.McpTeamModeFull
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled, mode) VALUES ($1, $2, $3, $4)
ON CONFLICT (team_id, server_id) DO UPDATE SET enabled = EXCLUDED.enabled, mode = EXCLUDED.mode,
  enabled_by = afs.current_actor(), enabled_at = now()`, sw.TeamID, sw.ServerID, sw.Enabled, string(mode))
	return MapError(err, OpWrite, "MCP server switch")
}

// TeamEnabledServers implements mcpcatalog.Repository.
func (*McpRepo) TeamEnabledServers(ctx context.Context, tx port.Tx, teamID string) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	err = collect(ctx, pt, "MCP server switch", `SELECT server_id FROM afs.team_mcp_servers WHERE team_id = $1 AND enabled ORDER BY server_id`,
		[]any{teamID}, func(rows pgx.Rows) error {
			var id string
			err := rows.Scan(&id)
			out = append(out, id)
			return err
		})
	return out, err
}

//nolint:gosec // a column list: credential metadata, never a value
const mcpCredentialSelect = `SELECT id, server_id, scope, coalesce(team_id, ''), kind, secret_arn, coalesce(subject, ''), scopes, expires_at,
  status, rotated_at, version FROM afs.mcp_credentials`

// GetCredential implements mcpcatalog.Repository: nil when the scope has none.
func (*McpRepo) GetCredential(ctx context.Context, tx port.Tx, serverID, scope string) (*domain.McpCredentialRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var c domain.McpCredentialRecord
	err = pt.QueryRow(ctx, mcpCredentialSelect+` WHERE server_id = $1 AND scope = $2`, serverID, scope).Scan(&c.ID, &c.ServerID,
		&c.Scope, &c.TeamID, &c.Kind, &c.SecretARN, &c.Subject, &c.Scopes, &c.ExpiresAt, &c.Status, &c.RotatedAt, &c.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpRead, "MCP credential")
	}
	return &c, nil
}

// CredentialPath implements mcpcatalog.Repository: afs.mcp_credential_path (teamID "" is the shared path).
func (*McpRepo) CredentialPath(ctx context.Context, tx port.Tx, teamID, slug string) (string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var p string
	err = pt.QueryRow(ctx, `SELECT afs.mcp_credential_path($1, $2)`, nullIfEmpty(teamID), slug).Scan(&p)
	return p, MapError(err, OpRead, "team")
}

// UpsertCredential implements mcpcatalog.Repository.
func (*McpRepo) UpsertCredential(ctx context.Context, tx port.Tx, c domain.McpCredentialRecord) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	var team any
	if c.Scope != domain.McpSharedScope {
		team = c.Scope
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.mcp_credentials (id, server_id, scope, team_id, kind, secret_arn, subject, scopes, expires_at, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (server_id, scope) DO UPDATE SET kind = EXCLUDED.kind, secret_arn = EXCLUDED.secret_arn, subject = EXCLUDED.subject,
  scopes = EXCLUDED.scopes, expires_at = EXCLUDED.expires_at, status = EXCLUDED.status, rotated_at = now()`,
		c.ID, c.ServerID, c.Scope, team, c.Kind, c.SecretARN, nullIfEmpty(c.Subject), orEmpty(c.Scopes), c.ExpiresAt, c.Status)
	return MapError(err, OpWrite, "MCP credential")
}

// DeleteCredential implements mcpcatalog.Repository.
func (*McpRepo) DeleteCredential(ctx context.Context, tx port.Tx, serverID, scope string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `DELETE FROM afs.mcp_credentials WHERE server_id = $1 AND scope = $2`, serverID, scope)
	return MapError(err, OpDelete, "MCP credential")
}

// RequestFederationTest implements mcpcatalog.Repository: the gateway tests the exchange on its next pass.
func (*McpRepo) RequestFederationTest(ctx context.Context, tx port.Tx, serverID, teamID string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.mcp_federation_states (server_id, team_id, test_requested_at) VALUES ($1, $2, now())
ON CONFLICT (server_id, team_id) DO UPDATE SET test_requested_at = now()`, serverID, teamID)
	return MapError(err, OpWrite, "MCP federation state")
}

// subjectTable names an agent or pool's table and its service account prefix.
func subjectTable(kind string) (table, prefix string, err error) {
	switch kind {
	case "agent":
		return "agents", "agent-", nil
	case "pool":
		return "agent_pools", "pool-", nil
	}
	return "", "", domain.Validation(domain.FieldError{Field: "kind", Reason: "must be agent or pool"})
}

// Subject implements mcpcatalog.Repository.
func (*McpRepo) Subject(ctx context.Context, tx port.Tx, teamID, kind, id string) (domain.McpSubject, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.McpSubject{}, err
	}
	table, _, err := subjectTable(kind)
	if err != nil {
		return domain.McpSubject{}, err
	}
	s := domain.McpSubject{Kind: kind, TeamID: teamID, ID: id}
	var mode string
	err = pt.QueryRow(ctx, `SELECT mcp_access, coalesce(mcp_mode, '') FROM afs.`+table+` WHERE team_id = $1 AND id = $2 AND deleted_at IS NULL`,
		teamID, id).Scan(&s.Access, &mode)
	s.Mode = domain.McpAgentMode(mode)
	return s, MapError(err, OpRead, kind)
}

// SubjectAccount implements mcpcatalog.Repository: the namespace and the service account the reconciler
// renders (agent-<name>, pool-<name>; afs.resolve_workload keys on the same).
func (*McpRepo) SubjectAccount(ctx context.Context, tx port.Tx, teamID, kind, id string) (string, string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", "", err
	}
	table, prefix, err := subjectTable(kind)
	if err != nil {
		return "", "", err
	}
	var ns, name string
	err = pt.QueryRow(ctx, `SELECT t.namespace, x.name FROM afs.`+table+` x JOIN afs.teams t ON t.id = x.team_id
WHERE x.team_id = $1 AND x.id = $2 AND x.deleted_at IS NULL`, teamID, id).Scan(&ns, &name)
	if err != nil {
		return "", "", MapError(err, OpRead, kind)
	}
	return ns, prefix + name, nil
}

// Admission implements mcpcatalog.Repository through afs.mcp_admission, which runs with the caller's rights.
func (*McpRepo) Admission(ctx context.Context, tx port.Tx, kind, id string) ([]domain.McpAdmissionRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []domain.McpAdmissionRow{}
	err = collect(ctx, pt, "MCP admission", `SELECT server_id, slug, server_kind, mode, primitive_id, kind, name, digest, allowed,
  coalesce(level, ''), coalesce(reason, ''), rate_limit_per_minute FROM afs.mcp_admission($1, $2)`, []any{kind, id},
		func(rows pgx.Rows) error {
			var r domain.McpAdmissionRow
			err := rows.Scan(&r.ServerID, &r.Slug, &r.ServerKind, &r.Mode, &r.PrimitiveID, &r.Kind, &r.Name, &r.Digest, &r.Allowed,
				&r.Level, &r.Reason, &r.RateLimit)
			out = append(out, r)
			return err
		})
	return out, err
}

const mcpEventCols = `e.id, e.at, e.request_id, e.team_id, coalesce(e.agent_id, ''), coalesce(e.pool_id, ''), coalesce(e.worker_pod, ''),
  coalesce(e.server_id, ''), coalesce(e.server_kind, ''), e.method, coalesce(e.name, ''), coalesce(e.primitive_id, ''),
  coalesce(e.digest, ''), e.outcome, coalesce(e.reason, ''), e.jsonrpc_code, e.http_status, e.gateway_ms, e.upstream_ms,
  e.request_bytes, e.response_bytes, coalesce(e.auth_kind, ''), coalesce(e.protocol_downstream, ''),
  coalesce(e.protocol_upstream, ''), coalesce(e.args_sha256, ''), e.search_results, e.search_hit, coalesce(e.query_text, '')`

func scanMcpEvent(rows pgx.Rows) (domain.McpEventRecord, error) {
	var (
		e    domain.McpEventRecord
		http *int16
	)
	err := rows.Scan(&e.ID, &e.At, &e.RequestID, &e.TeamID, &e.AgentID, &e.PoolID, &e.WorkerPod, &e.ServerID, &e.ServerKind, &e.Method,
		&e.Name, &e.PrimitiveID, &e.Digest, &e.Outcome, &e.Reason, &e.JSONRPCCode, &http, &e.GatewayMs, &e.UpstreamMs, &e.RequestBytes,
		&e.ResponseBytes, &e.AuthKind, &e.ProtocolDownstream, &e.ProtocolUpstream, &e.ArgsSHA256, &e.SearchResults, &e.SearchHit, &e.QueryText)
	if http != nil {
		v := int(*http)
		e.HTTPStatus = &v
	}
	return e, err
}

// parseEventCursor reads EventCursor's "time|id".
func parseEventCursor(c string) (time.Time, int64, error) {
	at, id, ok := strings.Cut(c, "|")
	if !ok {
		return time.Time{}, 0, domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid token"})
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, 0, domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid token"})
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return time.Time{}, 0, domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid token"})
	}
	return t, n, nil
}

// ListEvents implements mcpcatalog.Repository. Service reads go through afs.mcp_events_for_review, whose own
// limit (at most 1000) is applied before the filters it does not take, so the window is narrowed to the
// cursor first.
func (*McpRepo) ListEvents(ctx context.Context, tx port.Tx, f domain.McpEventFilter, service bool, cursor string, limit int) ([]domain.McpEventRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	to, cursorAt, cursorID := f.To, time.Time{}, int64(0)
	if cursor != "" {
		at, id, err := parseEventCursor(cursor)
		if err != nil {
			return nil, err
		}
		cursorAt, cursorID = at, id
		if next := at.Add(time.Microsecond); next.Before(to) {
			to = next
		}
	}
	args := []any{f.From, to}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	var q string
	if service {
		q = `SELECT ` + mcpEventCols + ` FROM afs.mcp_events_for_review($1, $2, ` + arg(nullIfEmpty(f.TeamID)) + `, ` +
			arg(nullIfEmpty(f.ServerID)) + `, 1000) e WHERE true`
	} else {
		q = `SELECT ` + mcpEventCols + ` FROM afs.mcp_events e WHERE e.at >= $1 AND e.at < $2`
		if f.TeamID != "" {
			q += ` AND e.team_id = ` + arg(f.TeamID)
		}
		if f.ServerID != "" {
			q += ` AND e.server_id = ` + arg(f.ServerID)
		}
	}
	for _, c := range []struct{ col, v string }{{"agent_id", f.AgentID}, {"pool_id", f.PoolID}, {"method", f.Method}, {"name", f.Name},
		{"outcome", f.Outcome}, {"reason", f.Reason}} {
		if c.v != "" {
			q += ` AND e.` + c.col + ` = ` + arg(c.v)
		}
	}
	if cursor != "" {
		q += ` AND (e.at, e.id) < (` + arg(cursorAt) + `, ` + arg(cursorID) + `)`
	}
	q += ` ORDER BY e.at DESC, e.id DESC LIMIT ` + arg(limit)
	out := []domain.McpEventRecord{}
	err = collect(ctx, pt, "MCP event", q, args, func(rows pgx.Rows) error {
		e, err := scanMcpEvent(rows)
		out = append(out, e)
		return err
	})
	return out, err
}

// usageKeys maps groupBy to the key and label expressions of the usage report.
var usageKeys = map[string][2]string{
	"server":    {`coalesce(e.server_id, '')`, `coalesce(max(s.slug), max(e.server_id), '')`},
	"primitive": {`coalesce(e.server_id, '') || '/' || coalesce(e.name, '')`, `coalesce(max(s.slug), '') || '.' || coalesce(max(e.name), '')`},
	"team":      {`e.team_id`, `coalesce(max(t.slug), e.team_id)`},
	"agent":     {`coalesce(e.agent_id, e.pool_id, '')`, `coalesce(e.agent_id, e.pool_id, '')`},
	"day":       {`to_char(date_trunc('day', e.at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`, `to_char(date_trunc('day', e.at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`},
}

// UsageReport implements mcpcatalog.Repository from the ledger itself (the daily rollup lags a day). The
// unused view lists approved primitives with no call in the last WindowDays days, whatever groupBy says.
func (*McpRepo) UsageReport(ctx context.Context, tx port.Tx, q domain.McpUsageQuery) ([]domain.McpUsageRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	if q.View == "unused" {
		return unusedPrimitives(ctx, pt, q)
	}
	k, ok := usageKeys[q.GroupBy]
	if !ok {
		return nil, domain.Validation(domain.FieldError{Field: "groupBy", Reason: "unknown grouping"})
	}
	where := `e.at >= $1 AND e.at < $2 AND ($3 = '' OR e.team_id = $3)`
	switch q.View {
	case "refusals":
		where += ` AND e.outcome = 'refused'`
	case "discovery":
		where += ` AND e.search_hit IS NOT NULL`
	}
	sql := `SELECT ` + k[0] + `, ` + k[1] + `, count(*), count(*) FILTER (WHERE e.outcome NOT IN ('ok', 'refused')),
  count(*) FILTER (WHERE e.outcome = 'refused'),
  percentile_disc(0.5) WITHIN GROUP (ORDER BY coalesce(e.gateway_ms, 0) + coalesce(e.upstream_ms, 0)),
  percentile_disc(0.95) WITHIN GROUP (ORDER BY coalesce(e.gateway_ms, 0) + coalesce(e.upstream_ms, 0)),
  count(DISTINCT e.team_id), count(DISTINCT coalesce(e.agent_id, e.pool_id)),
  count(*) FILTER (WHERE e.search_hit IS NOT NULL), avg(CASE WHEN e.search_hit THEN 1.0 WHEN NOT e.search_hit THEN 0.0 END), max(e.at)
FROM afs.mcp_events e LEFT JOIN afs.mcp_servers s ON s.id = e.server_id LEFT JOIN afs.teams t ON t.id = e.team_id
WHERE ` + where + ` GROUP BY 1 ORDER BY 3 DESC, 1 LIMIT 1000`
	out := []domain.McpUsageRow{}
	err = collect(ctx, pt, "MCP usage", sql, []any{q.From, q.To, q.TeamID}, func(rows pgx.Rows) error {
		var (
			r             domain.McpUsageRow
			p50, p95      *int64
			teams, agents int
			searches      int64
			hitRate       *float64
			lastUsed      time.Time
			label         string
		)
		if err := rows.Scan(&r.Key, &label, &r.Calls, &r.Errors, &r.Refusals, &p50, &p95, &teams, &agents, &searches, &hitRate, &lastUsed); err != nil {
			return err
		}
		r.Label = label
		r.P50Ms, r.P95Ms = intPtr(p50), intPtr(p95)
		r.LastUsedAt = &lastUsed
		switch q.View {
		case "adoption":
			r.Teams, r.Agents = &teams, &agents
		case "discovery":
			r.Searches, r.SearchHitRate = &searches, hitRate
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func intPtr(p *int64) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func unusedPrimitives(ctx context.Context, pt *Tx, q domain.McpUsageQuery) ([]domain.McpUsageRow, error) {
	out := []domain.McpUsageRow{}
	err := collect(ctx, pt, "MCP usage", `SELECT p.server_id || '/' || p.name, s.slug || '.' || p.name,
  (SELECT max(e.at) FROM afs.mcp_events e WHERE e.server_id = p.server_id AND e.name = p.name AND ($3 = '' OR e.team_id = $3))
FROM afs.mcp_primitives p JOIN afs.mcp_servers s ON s.id = p.server_id
WHERE p.removed_at IS NULL AND p.status = 'approved'
  AND ($3 = '' OR s.team_id = $3 OR EXISTS (SELECT 1 FROM afs.team_mcp_servers x WHERE x.team_id = $3 AND x.server_id = s.id AND x.enabled))
  AND NOT EXISTS (SELECT 1 FROM afs.mcp_events e WHERE e.at >= $1 AND e.at < $2 AND e.server_id = p.server_id AND e.name = p.name
                    AND ($3 = '' OR e.team_id = $3))
ORDER BY 2 LIMIT 1000`, []any{q.To.AddDate(0, 0, -q.WindowDays), q.To, q.TeamID}, func(rows pgx.Rows) error {
		var r domain.McpUsageRow
		err := rows.Scan(&r.Key, &r.Label, &r.LastUsedAt)
		out = append(out, r)
		return err
	})
	return out, err
}

const mcpCaptureSelect = `SELECT id, started_by, started_at, until, reason, stopped_at, coalesce(stopped_by, '') FROM afs.mcp_query_captures`

func getCapture(ctx context.Context, tx port.Tx, where string) (*domain.McpQueryCapture, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var c domain.McpQueryCapture
	err = pt.QueryRow(ctx, mcpCaptureSelect+` WHERE `+where+` ORDER BY started_at DESC LIMIT 1`).Scan(&c.ID, &c.StartedBy, &c.StartedAt,
		&c.Until, &c.Reason, &c.StoppedAt, &c.StoppedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpRead, "query capture")
	}
	return &c, nil
}

// OpenCapture implements mcpcatalog.Repository.
func (*McpRepo) OpenCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error) {
	return getCapture(ctx, tx, `stopped_at IS NULL AND until > now()`)
}

// LatestCapture implements mcpcatalog.Repository.
func (*McpRepo) LatestCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error) {
	return getCapture(ctx, tx, `stopped_at IS NULL`)
}

// InsertCapture implements mcpcatalog.Repository.
func (*McpRepo) InsertCapture(ctx context.Context, tx port.Tx, c domain.McpQueryCapture) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.mcp_query_captures (id, reason, started_at, until) VALUES ($1, $2, $3, $4)`,
		c.ID, c.Reason, c.StartedAt, c.Until)
	return MapError(err, OpWrite, "query capture")
}

// StopCapture implements mcpcatalog.Repository.
func (*McpRepo) StopCapture(ctx context.Context, tx port.Tx, id string, at time.Time, by string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.mcp_query_captures SET stopped_at = $2, stopped_by = $3 WHERE id = $1 AND stopped_at IS NULL`, id, at, by)
	return MapError(err, OpWrite, "query capture")
}

// fedTable names a federated identity table and its subject column.
func fedTable(kind string) (string, string, error) {
	switch kind {
	case "agent":
		return "agent_federated_identities", "agent_id", nil
	case "pool":
		return "pool_federated_identities", "pool_id", nil
	}
	return "", "", domain.Validation(domain.FieldError{Field: "kind", Reason: "must be agent or pool"})
}

func scanFederated(row pgx.Row) (domain.FederatedIdentityRecord, error) {
	var f domain.FederatedIdentityRecord
	err := row.Scan(&f.TeamID, &f.SubjectID, &f.Name, &f.ID, &f.Audience, &f.FilePath, &f.EnvVar, &f.ExpirationSeconds, &f.Version,
		&f.CreatedAt, &f.CreatedBy)
	return f, err
}

func fedSelect(table, col string) string {
	return `SELECT team_id, ` + col + `, name, id, audience, file_path, env_var, expiration_seconds, version, created_at, created_by
FROM afs.` + table + ` WHERE team_id = $1 AND ` + col + ` = $2`
}

// ListFederated implements mcpcatalog.Repository.
func (*McpRepo) ListFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID string) ([]domain.FederatedIdentityRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	table, col, err := fedTable(kind)
	if err != nil {
		return nil, err
	}
	out := []domain.FederatedIdentityRecord{}
	err = collect(ctx, pt, "federated identity", fedSelect(table, col)+` ORDER BY name`, []any{teamID, subjectID}, func(rows pgx.Rows) error {
		f, err := scanFederated(rows)
		out = append(out, f)
		return err
	})
	return out, err
}

// GetFederated implements mcpcatalog.Repository: nil when there is none.
func (*McpRepo) GetFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string) (*domain.FederatedIdentityRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	table, col, err := fedTable(kind)
	if err != nil {
		return nil, err
	}
	f, err := scanFederated(pt.QueryRow(ctx, fedSelect(table, col)+` AND name = $3`, teamID, subjectID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpRead, "federated identity")
	}
	return &f, nil
}

// InsertFederated implements mcpcatalog.Repository; an empty file path or variable takes the database's default.
func (*McpRepo) InsertFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	table, col, err := fedTable(kind)
	if err != nil {
		return err
	}
	exp := f.ExpirationSeconds
	if exp == 0 {
		exp = 3600
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.`+table+` (team_id, `+col+`, name, id, audience, file_path, env_var, expiration_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, f.TeamID, f.SubjectID, f.Name, f.ID, f.Audience, nullIfEmpty(f.FilePath), nullIfEmpty(f.EnvVar), exp)
	return MapError(err, OpWrite, "federated identity")
}

// UpdateFederated implements mcpcatalog.Repository.
func (*McpRepo) UpdateFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	table, col, err := fedTable(kind)
	if err != nil {
		return err
	}
	exp := f.ExpirationSeconds
	if exp == 0 {
		exp = 3600
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.`+table+` SET audience = $4, file_path = $5, env_var = $6, expiration_seconds = $7
WHERE team_id = $1 AND `+col+` = $2 AND name = $3 AND version = $8`, f.TeamID, f.SubjectID, f.Name, f.Audience, nullIfEmpty(f.FilePath),
		nullIfEmpty(f.EnvVar), exp, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "federated identity")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// DeleteFederated implements mcpcatalog.Repository.
func (*McpRepo) DeleteFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	table, col, err := fedTable(kind)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.`+table+` WHERE team_id = $1 AND `+col+` = $2 AND name = $3 AND version = $4`,
		teamID, subjectID, name, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "federated identity")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// InsertOAuthState implements mcpcatalog.Repository: only the state's hash is stored, and no verifier (000026).
func (*McpRepo) InsertOAuthState(ctx context.Context, tx port.Tx, hash []byte, teamID, serverID string, expiresAt time.Time) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.mcp_oauth_states (state_hash, team_id, server_id, expires_at) VALUES ($1, $2, $3, $4)`,
		hash, teamID, serverID, expiresAt)
	return MapError(err, OpWrite, "MCP OAuth state")
}

// ClaimOAuthState implements mcpcatalog.Repository through afs.claim_mcp_oauth_state (single use, SECURITY DEFINER).
func (*McpRepo) ClaimOAuthState(ctx context.Context, tx port.Tx, hash []byte) (*mcpcatalog.OAuthState, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var st mcpcatalog.OAuthState
	err = pt.QueryRow(ctx, `SELECT team_id, server_id, created_by FROM afs.claim_mcp_oauth_state($1)`, hash).
		Scan(&st.TeamID, &st.ServerID, &st.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpWrite, "MCP OAuth state")
	}
	return &st, nil
}
