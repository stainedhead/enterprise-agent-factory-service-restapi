package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// McpRepo implements mcpcatalog.Repository on the 000024 tables as afs_app. Row-level security decides what
// a session sees: the enterprise catalog is readable by everyone, enterprise rows change only for service
// admins (and security operators' reviews), team rows only for their team.
type McpRepo struct{}

// NewMcpRepo returns an McpRepo.
func NewMcpRepo() *McpRepo { return &McpRepo{} }

var _ mcpcatalog.Repository = (*McpRepo)(nil)

// mcpSettingKeys are the service settings the catalog reads, through afs.mcp_setting (which afs_app may call).
var mcpSettingKeys = []string{
	domain.SettingMcpEnabled, domain.SettingMcpTeamServerApproval, domain.SettingMcpHostDenyList,
	domain.SettingMcpCorporateDomains, domain.SettingMcpAllowedImageRegistries, domain.SettingMcpImageSignaturePolicy,
	domain.SettingMcpDefaultAgentMode, domain.SettingMcpQueryCaptureMaxDays, domain.SettingFederationAllowedAudiences,
}

// Settings implements mcpcatalog.Repository. A key with no row takes its PRD default.
func (*McpRepo) Settings(ctx context.Context, tx port.Tx) (mcpcatalog.Settings, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return mcpcatalog.Settings{}, err
	}
	vals := map[string]json.RawMessage{}
	for _, k := range mcpSettingKeys {
		if spec, ok := domain.SettingSpecFor(k); ok {
			vals[k] = json.RawMessage(spec.Default)
		}
	}
	err = collect(ctx, pt, "service settings", `SELECT k, afs.mcp_setting(k) FROM unnest($1::text[]) k`, []any{mcpSettingKeys},
		func(rows pgx.Rows) error {
			var (
				k string
				v []byte
			)
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			if v != nil {
				vals[k] = v
			}
			return nil
		})
	if err != nil {
		return mcpcatalog.Settings{}, err
	}
	var st mcpcatalog.Settings
	var mode string
	for _, d := range []struct {
		key string
		dst any
	}{
		{domain.SettingMcpEnabled, &st.Enabled}, {domain.SettingMcpTeamServerApproval, &st.Approval},
		{domain.SettingMcpHostDenyList, &st.HostDenyList}, {domain.SettingMcpCorporateDomains, &st.CorporateDomains},
		{domain.SettingMcpAllowedImageRegistries, &st.AllowedRegistries}, {domain.SettingMcpImageSignaturePolicy, &st.SignaturePolicy},
		{domain.SettingMcpDefaultAgentMode, &mode}, {domain.SettingMcpQueryCaptureMaxDays, &st.CaptureMaxDays},
		{domain.SettingFederationAllowedAudiences, &st.AllowedAudiences},
	} {
		if raw, ok := vals[d.key]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, d.dst); err != nil {
				return mcpcatalog.Settings{}, domain.Internal(err)
			}
		}
	}
	st.DefaultAgentMode = domain.McpAgentMode(mode)
	return st, nil
}

const mcpServerSelect = `SELECT s.id, coalesce(s.team_id, ''), s.slug, s.display_name, s.owner, s.purpose, coalesce(s.doc_url, ''),
  s.data_classification, s.transport, s.location, coalesce(s.url, ''), coalesce(s.ca_bundle, ''), s.auth_kind,
  coalesce(s.auth_header, ''), s.credential_mode, s.federation, coalesce(s.protocol_version, ''), s.review_status,
  coalesce(s.suspended_from, ''), s.published, coalesce(s.risk_tier, ''), s.risk_reasons, s.checks, s.drift_policy,
  s.default_for_new_teams, s.allow_sampling, s.trust_annotations, s.limits, coalesce(s.suspended_reason, ''),
  s.version, s.generation, s.created_at, s.created_by, s.updated_at,
  h.image, h.args, h.env_names, h.instance_mode, h.egress_hosts, h.resources, h.needs_aws
FROM afs.mcp_servers s LEFT JOIN afs.mcp_hosted_specs h ON h.server_id = s.id`

func mcpServerDest(s *domain.McpServerRecord, raw *mcpServerRaw) []any {
	return []any{&s.ID, &s.TeamID, &s.Slug, &s.DisplayName, &s.Owner, &s.Purpose, &s.DocURL,
		&s.DataClassification, &s.Transport, &s.Location, &s.URL, &s.CABundle, &s.AuthKind,
		&s.AuthHeader, &s.CredentialMode, &raw.federation, &s.ProtocolVersion, &s.ReviewStatus,
		&s.SuspendedFrom, &s.Published, &s.RiskTier, &s.RiskReasons, &raw.checks, &s.DriftPolicy,
		&s.DefaultForNewTeams, &s.AllowSampling, &s.TrustAnnotations, &raw.limits, &s.SuspendedReason,
		&s.Version, &s.Generation, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt,
		&raw.image, &raw.args, &raw.envNames, &raw.instanceMode, &raw.egress, &raw.resources, &raw.needsAWS}
}

type mcpServerRaw struct {
	federation, checks, limits, resources []byte
	image, instanceMode                   *string
	args, envNames, egress                []string
	needsAWS                              *bool
}

func (raw mcpServerRaw) finish(s *domain.McpServerRecord) error {
	if len(raw.federation) > 0 && string(raw.federation) != "null" {
		s.Federation = &domain.McpFederation{}
		if err := json.Unmarshal(raw.federation, s.Federation); err != nil {
			return err
		}
	}
	if len(raw.checks) > 0 {
		if err := json.Unmarshal(raw.checks, &s.Checks); err != nil {
			return err
		}
	}
	if len(raw.limits) > 0 {
		if err := json.Unmarshal(raw.limits, &s.Limits); err != nil {
			return err
		}
	}
	if raw.image != nil {
		h := &domain.McpHosted{Image: *raw.image, Args: raw.args, EnvNames: raw.envNames, EgressHosts: raw.egress}
		h.InstanceMode = deref(raw.instanceMode)
		h.NeedsAWS = raw.needsAWS != nil && *raw.needsAWS
		if len(raw.resources) > 0 && string(raw.resources) != "null" {
			if err := json.Unmarshal(raw.resources, &h.Resources); err != nil {
				return err
			}
		}
		s.Hosted = h
	}
	if s.RiskReasons == nil {
		s.RiskReasons = []string{}
	}
	return nil
}

func scanMcpServer(row pgx.Row, extra ...any) (domain.McpServerRecord, error) {
	var (
		s   domain.McpServerRecord
		raw mcpServerRaw
	)
	if err := row.Scan(append(mcpServerDest(&s, &raw), extra...)...); err != nil {
		return s, err
	}
	if err := raw.finish(&s); err != nil {
		return s, domain.Internal(err)
	}
	return s, nil
}

// ListServers implements mcpcatalog.Repository.
func (*McpRepo) ListServers(ctx context.Context, tx port.Tx, f mcpcatalog.ServerFilter, after string, limit int) ([]domain.McpServerRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	q := mcpServerSelect + ` WHERE s.id > $1
  AND ($2 = '' OR ($2 = 'enterprise' AND s.team_id IS NULL) OR ($2 = 'team' AND s.team_id IS NOT NULL))
  AND ($3 = '' OR s.review_status = $3) AND ($4 = '' OR s.risk_tier = $4) AND ($5 = '' OR s.team_id = $5)
  AND ($6 = '' OR s.location = $6)
ORDER BY s.id LIMIT $7`
	out := []domain.McpServerRecord{}
	err = collect(ctx, pt, "MCP server", q, []any{after, f.Scope, f.ReviewStatus, f.RiskTier, f.TeamID, f.Location, limit},
		func(rows pgx.Rows) error {
			s, err := scanMcpServer(rows)
			out = append(out, s)
			return err
		})
	return out, err
}

// GetServer implements mcpcatalog.Repository.
func (*McpRepo) GetServer(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.McpServerRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.McpServerRecord{}, err
	}
	q := mcpServerSelect + ` WHERE s.id = $1`
	if forUpdate {
		// FOR UPDATE applies the UPDATE policies too, so a team locking an enterprise row it may only read sees
		// nothing; that caller changes the team's own rows, so it reads the server unlocked.
		s, err := scanMcpServer(pt.QueryRow(ctx, q+` FOR UPDATE OF s`, id))
		if !errors.Is(err, pgx.ErrNoRows) {
			return s, MapError(err, OpRead, "MCP server")
		}
	}
	s, err := scanMcpServer(pt.QueryRow(ctx, q, id))
	return s, MapError(err, OpRead, "MCP server")
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func jsonArg(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", domain.Internal(err)
	}
	return string(b), nil
}

// serverArgs are the mutable columns in the order of insertServerSQL's $2..$24.
func serverArgs(s domain.McpServerRecord) ([]any, error) {
	var fed any
	if s.Federation != nil {
		b, err := jsonArg(s.Federation)
		if err != nil {
			return nil, err
		}
		fed = b
	}
	checks := s.Checks
	if checks == nil {
		checks = []domain.McpCheck{}
	}
	cj, err := jsonArg(checks)
	if err != nil {
		return nil, err
	}
	lj, err := jsonArg(s.Limits)
	if err != nil {
		return nil, err
	}
	return []any{nullIfEmpty(s.TeamID), s.Slug, s.DisplayName, s.Owner, s.Purpose, nullIfEmpty(s.DocURL), s.DataClassification,
		string(s.Transport), string(s.Location), nullIfEmpty(s.URL), nullIfEmpty(s.CABundle), string(s.AuthKind),
		nullIfEmpty(s.AuthHeader), s.CredentialMode, fed, nullIfEmpty(s.ProtocolVersion), string(s.ReviewStatus), s.Published,
		nullIfEmpty(s.RiskTier), orEmpty(s.RiskReasons), cj, s.DriftPolicy, s.DefaultForNewTeams, s.AllowSampling,
		s.TrustAnnotations, lj, nullIfEmpty(s.SuspendedReason)}, nil
}

const insertServerSQL = `INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, doc_url, data_classification,
  transport, location, url, ca_bundle, auth_kind, auth_header, credential_mode, federation, protocol_version, review_status,
  published, risk_tier, risk_reasons, checks, drift_policy, default_for_new_teams, allow_sampling, trust_annotations, limits,
  suspended_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb, $17, $18, $19, $20, $21, $22::jsonb, $23,
  $24, $25, $26, $27::jsonb, $28)`

// InsertServer implements mcpcatalog.Repository.
func (*McpRepo) InsertServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	args, err := serverArgs(s)
	if err != nil {
		return err
	}
	if _, err := pt.Exec(ctx, insertServerSQL, append([]any{s.ID}, args...)...); err != nil {
		return MapError(err, OpWrite, "MCP server")
	}
	return putHosted(ctx, pt, s)
}

const updateServerSQL = `UPDATE afs.mcp_servers SET team_id = $2, slug = $3, display_name = $4, owner = $5, purpose = $6, doc_url = $7,
  data_classification = $8, transport = $9, location = $10, url = $11, ca_bundle = $12, auth_kind = $13, auth_header = $14,
  credential_mode = $15, federation = $16::jsonb, protocol_version = $17, review_status = $18, published = $19, risk_tier = $20,
  risk_reasons = $21, checks = $22::jsonb, drift_policy = $23, default_for_new_teams = $24, allow_sampling = $25,
  trust_annotations = $26, limits = $27::jsonb, suspended_reason = $28
WHERE id = $1 AND version = $29`

// UpdateServer implements mcpcatalog.Repository. suspended_from and generation are the database's to keep.
func (*McpRepo) UpdateServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	args, err := serverArgs(s)
	if err != nil {
		return err
	}
	args = append(append([]any{s.ID}, args...), int64(expect))
	tag, err := pt.Exec(ctx, updateServerSQL, args...)
	if err != nil {
		return MapError(err, OpWrite, "MCP server")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return putHosted(ctx, pt, s)
}

func putHosted(ctx context.Context, pt *Tx, s domain.McpServerRecord) error {
	if s.Hosted == nil {
		_, err := pt.Exec(ctx, `DELETE FROM afs.mcp_hosted_specs WHERE server_id = $1`, s.ID)
		return MapError(err, OpDelete, "MCP server")
	}
	h := s.Hosted
	var res any
	if len(h.Resources) > 0 {
		b, err := jsonArg(h.Resources)
		if err != nil {
			return err
		}
		res = b
	}
	mode := h.InstanceMode
	if mode == "" {
		mode = "per_team"
	}
	_, err := pt.Exec(ctx, `INSERT INTO afs.mcp_hosted_specs (server_id, team_id, image, args, env_names, instance_mode, egress_hosts,
  resources, needs_aws) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)
ON CONFLICT (server_id) DO UPDATE SET image = EXCLUDED.image, args = EXCLUDED.args, env_names = EXCLUDED.env_names,
  instance_mode = EXCLUDED.instance_mode, egress_hosts = EXCLUDED.egress_hosts, resources = EXCLUDED.resources,
  needs_aws = EXCLUDED.needs_aws`,
		s.ID, nullIfEmpty(s.TeamID), h.Image, orEmpty(h.Args), orEmpty(h.EnvNames), mode, orEmpty(h.EgressHosts), res, h.NeedsAWS)
	return MapError(err, OpWrite, "MCP server")
}

// DeleteServer implements mcpcatalog.Repository.
func (*McpRepo) DeleteServer(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.mcp_servers WHERE id = $1 AND version = $2`, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "MCP server")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// ServerInUse implements mcpcatalog.Repository. Enterprise servers change only for service admins, who see
// every team's switch; a team server is switched on only by its own team.
func (*McpRepo) ServerInUse(ctx context.Context, tx port.Tx, id string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var n int
	err = pt.QueryRow(ctx, `SELECT count(*) FROM afs.team_mcp_servers WHERE server_id = $1 AND enabled`, id).Scan(&n)
	return n > 0, MapError(err, OpRead, "MCP server")
}

// Adoption implements mcpcatalog.Repository: teams with the server on, and their live agents and pools that
// reach it (all servers, or this one selected).
func (*McpRepo) Adoption(ctx context.Context, tx port.Tx, id string) (mcpcatalog.Adoption, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return mcpcatalog.Adoption{}, err
	}
	var a mcpcatalog.Adoption
	err = pt.QueryRow(ctx, `WITH on_teams AS (
  SELECT team_id FROM afs.team_mcp_servers WHERE server_id = $1 AND enabled
)
SELECT (SELECT count(*) FROM on_teams),
  (SELECT count(*) FROM afs.agents a JOIN on_teams t ON t.team_id = a.team_id
    WHERE a.deleted_at IS NULL AND (a.mcp_access = 'all' OR EXISTS (SELECT 1 FROM afs.agent_mcp_servers x
      WHERE x.team_id = a.team_id AND x.agent_id = a.id AND x.server_id = $1)))
  + (SELECT count(*) FROM afs.agent_pools p JOIN on_teams t ON t.team_id = p.team_id
    WHERE p.deleted_at IS NULL AND (p.mcp_access = 'all' OR EXISTS (SELECT 1 FROM afs.pool_mcp_servers x
      WHERE x.team_id = p.team_id AND x.pool_id = p.id AND x.server_id = $1)))`, id).Scan(&a.Teams, &a.Agents)
	return a, MapError(err, OpRead, "MCP server")
}

// RequestRefresh implements mcpcatalog.Repository.
func (*McpRepo) RequestRefresh(ctx context.Context, tx port.Tx, id string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.mcp_servers SET refresh_requested_at = now() WHERE id = $1`, id)
	return MapError(err, OpWrite, "MCP server")
}

const mcpPrimitiveSelect = `SELECT p.id, p.server_id, coalesce(p.team_id, ''), p.kind, p.name, coalesce(p.title, ''), coalesce(p.summary, ''),
  p.annotations, p.definition, p.pending_definition, p.digest, coalesce(p.approved_digest, ''), coalesce(p.change, ''), p.status,
  coalesce(p.decided_by, ''), p.first_seen, p.last_seen
FROM afs.mcp_primitives p`

func scanMcpPrimitive(row pgx.Row) (domain.McpPrimitiveRecord, error) {
	var (
		p                  domain.McpPrimitiveRecord
		ann, def, pendingB []byte
	)
	err := row.Scan(&p.ID, &p.ServerID, &p.TeamID, &p.Kind, &p.Name, &p.Title, &p.Summary, &ann, &def, &pendingB, &p.Digest,
		&p.ApprovedDigest, &p.Change, &p.Status, &p.DecidedBy, &p.FirstSeen, &p.LastSeen)
	p.Annotations, p.Definition = json.RawMessage(ann), json.RawMessage(def)
	if pendingB != nil {
		p.PendingDefinition = json.RawMessage(pendingB)
	}
	return p, err
}

// ListPrimitives implements mcpcatalog.Repository: the live inventory, in id order.
func (*McpRepo) ListPrimitives(ctx context.Context, tx port.Tx, serverID string, f mcpcatalog.PrimitiveFilter, after string, limit int) ([]domain.McpPrimitiveRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []domain.McpPrimitiveRecord{}
	err = collect(ctx, pt, "MCP primitive", mcpPrimitiveSelect+` WHERE p.server_id = $1 AND p.removed_at IS NULL AND p.id > $2
  AND ($3 = '' OR p.kind = $3) AND ($4 = '' OR p.status = $4) ORDER BY p.id LIMIT $5`,
		[]any{serverID, after, f.Kind, f.Status, limit}, func(rows pgx.Rows) error {
			p, err := scanMcpPrimitive(rows)
			out = append(out, p)
			return err
		})
	return out, err
}

// GetPrimitive implements mcpcatalog.Repository.
func (*McpRepo) GetPrimitive(ctx context.Context, tx port.Tx, serverID, id string, forUpdate bool) (domain.McpPrimitiveRecord, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.McpPrimitiveRecord{}, err
	}
	q := mcpPrimitiveSelect + ` WHERE p.server_id = $1 AND p.id = $2 AND p.removed_at IS NULL`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanMcpPrimitive(pt.QueryRow(ctx, q, serverID, id))
	return p, MapError(err, OpRead, "MCP primitive")
}

// DecidePrimitive implements mcpcatalog.Repository; the guard adopts the held definition on approval.
func (*McpRepo) DecidePrimitive(ctx context.Context, tx port.Tx, id, status string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.mcp_primitives SET status = $2, decided_by = afs.current_actor(), decided_at = now() WHERE id = $1`,
		id, status)
	return MapError(err, OpWrite, "MCP primitive")
}

// InsertReview implements mcpcatalog.Repository.
func (*McpRepo) InsertReview(ctx context.Context, tx port.Tx, r domain.McpReview) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	checks := r.Checks
	if checks == nil {
		checks = []domain.McpCheck{}
	}
	cj, err := jsonArg(checks)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.mcp_reviews (id, server_id, team_id, primitive_id, at, checks, risk_tier, approver, decision, note)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10)`, r.ID, r.ServerID, nullIfEmpty(r.TeamID), nullIfEmpty(r.PrimitiveID), r.At, cj,
		nullIfEmpty(r.RiskTier), r.Approver, r.Decision, nullIfEmpty(r.Note))
	return MapError(err, OpWrite, "MCP review")
}

// ReviewQueue implements mcpcatalog.Repository through afs.mcp_review_queue (service admins and security
// operators only; the function refuses anyone else).
func (*McpRepo) ReviewQueue(ctx context.Context, tx port.Tx) ([]domain.McpReviewQueueItem, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []domain.McpReviewQueueItem{}
	err = collect(ctx, pt, "MCP review queue", `SELECT q.item_kind, q.server_id, coalesce(q.team_id, ''), coalesce(q.primitive_id, ''),
  q.slug, q.name, coalesce(q.risk_tier, ''), coalesce(s.risk_reasons, '{}'), q.approver, q.since
FROM afs.mcp_review_queue() q LEFT JOIN afs.mcp_servers s ON s.id = q.server_id ORDER BY q.since, q.server_id, q.primitive_id`, nil,
		func(rows pgx.Rows) error {
			var i domain.McpReviewQueueItem
			err := rows.Scan(&i.Kind, &i.ServerID, &i.TeamID, &i.PrimitiveID, &i.Slug, &i.Name, &i.RiskTier, &i.RiskReasons, &i.Approver, &i.Since)
			out = append(out, i)
			return err
		})
	return out, err
}

// policyWhere selects one scope's rules; $1 is the server.
func policyWhere(s mcpcatalog.PolicyScope) (string, []any) {
	args := []any{s.ServerID, s.Level}
	q := ` WHERE server_id = $1 AND level = $2`
	switch s.Level {
	case "team":
		q += ` AND team_id = $3`
		args = append(args, s.TeamID)
	case "agent":
		q += ` AND team_id = $3 AND agent_id = $4`
		args = append(args, s.TeamID, s.SubjectID)
	case "pool":
		q += ` AND team_id = $3 AND pool_id = $4`
		args = append(args, s.TeamID, s.SubjectID)
	}
	return q, args
}

// ListRules implements mcpcatalog.Repository: one scope's rules in insertion order.
func (*McpRepo) ListRules(ctx context.Context, tx port.Tx, s mcpcatalog.PolicyScope) ([]domain.McpPolicyRule, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	where, args := policyWhere(s)
	out := []domain.McpPolicyRule{}
	err = collect(ctx, pt, "MCP policy", `SELECT coalesce(name, ''), coalesce(class, ''), effect, rate_limit_per_minute FROM afs.mcp_policies`+
		where+` ORDER BY created_at, id`, args, func(rows pgx.Rows) error {
		var (
			r     domain.McpPolicyRule
			class string
		)
		err := rows.Scan(&r.Name, &class, &r.Effect, &r.RateLimitPerMinute)
		r.Class = mcp.Class(class)
		out = append(out, r)
		return err
	})
	return out, err
}

// ReplaceRules implements mcpcatalog.Repository.
func (*McpRepo) ReplaceRules(ctx context.Context, tx port.Tx, s mcpcatalog.PolicyScope, rules []domain.McpPolicyRule, ids []string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if len(ids) != len(rules) {
		return domain.Internal(errors.New("postgres: one id per MCP rule"))
	}
	where, args := policyWhere(s)
	if _, err := pt.Exec(ctx, `DELETE FROM afs.mcp_policies`+where, args...); err != nil {
		return MapError(err, OpDelete, "MCP policy")
	}
	var agent, pool any
	switch s.Level {
	case "agent":
		agent = s.SubjectID
	case "pool":
		pool = s.SubjectID
	}
	for i, r := range rules {
		_, err := pt.Exec(ctx, `INSERT INTO afs.mcp_policies (id, level, team_id, agent_id, pool_id, server_id, name, class, effect,
  rate_limit_per_minute, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, clock_timestamp())`,
			ids[i], s.Level, nullIfEmpty(s.TeamID), agent, pool, s.ServerID, nullIfEmpty(r.Name), nullIfEmpty(string(r.Class)), r.Effect,
			r.RateLimitPerMinute)
		if err != nil {
			return MapError(err, OpWrite, "MCP policy")
		}
	}
	return nil
}
