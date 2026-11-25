package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpup"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

// McpGatewayStore is the team gateway's MCP state (afs_gateway with afs.team_ids set, database contract §7.5): the
// admission view of a caller, the targets of its servers, credential and federation status, and the inventory
// refresh. It never reads a credential: those are mounted files.
type McpGatewayStore struct{ pool *pgxpool.Pool }

// NewMcpGatewayStore returns a store on the afs_gateway pool.
func NewMcpGatewayStore(pool *pgxpool.Pool) *McpGatewayStore { return &McpGatewayStore{pool: pool} }

var (
	_ mcpup.State        = (*McpGatewayStore)(nil)
	_ mcpup.RefreshStore = (*McpGatewayStore)(nil)
	_ mcpup.Targets      = (*McpGatewayStore)(nil)
)

// McpEnabled reads mcp.enabled; the gateway answers 404 on every MCP path when it is off.
func (g *McpGatewayStore) McpEnabled(ctx context.Context) (bool, error) {
	var on bool
	err := g.pool.QueryRow(ctx, `SELECT coalesce((afs.mcp_setting('mcp.enabled'))::boolean, false)`).Scan(&on)
	if err != nil {
		return false, fmt.Errorf("read mcp.enabled: %w", err)
	}
	return on, nil
}

// McpLimits reads the default per-server limits (mcp.limits) and the search cap (mcp.search_limit_max).
func (g *McpGatewayStore) McpLimits(ctx context.Context) (mcpgateway.Limits, int, error) {
	var raw []byte
	var max int
	err := g.pool.QueryRow(ctx, `SELECT coalesce(afs.mcp_setting('mcp.limits'), '{}'), coalesce((afs.mcp_setting('mcp.search_limit_max'))::int, 25)`).Scan(&raw, &max)
	if err != nil {
		return mcpgateway.Limits{}, 0, fmt.Errorf("read mcp.limits: %w", err)
	}
	return limitsOf(raw), max, nil
}

func limitsOf(raw []byte) mcpgateway.Limits {
	var l domain.McpLimits
	_ = json.Unmarshal(raw, &l)
	return mcpgateway.Limits{MaxRequestBytes: int64(l.RequestBytes), MaxResponseBytes: int64(l.ResponseBytes),
		Timeout: time.Duration(l.TimeoutSeconds) * time.Second, Concurrency: l.ConcurrentPerAgent}
}

// serverLevel are the refusal reasons that hide the whole server from the caller.
var serverLevel = map[string]bool{
	"server_not_published": true, "server_suspended": true, "server_not_enabled_for_team": true, "server_not_enabled_for_agent": true,
}

const mcpSnapshotSQL = `SELECT m.server_id, m.slug, m.server_kind, m.mode, m.kind, m.name, m.digest, m.allowed, coalesce(m.reason, ''),
  coalesce(m.rate_limit_per_minute, 0), p.definition, s.limits,
  coalesce((SELECT a.mcp_mode FROM afs.agents a WHERE $1 = 'agent' AND a.id = $2
            UNION ALL SELECT x.mcp_mode FROM afs.agent_pools x WHERE $1 = 'pool' AND x.id = $2 LIMIT 1),
           afs.mcp_setting('mcp.default_agent_mode') #>> '{}', 'search')
FROM afs.mcp_admission($1, $2) m
JOIN afs.mcp_primitives p ON p.id = m.primitive_id
JOIN afs.mcp_servers s ON s.id = m.server_id`

// Snapshot implements mcpgateway.Admission from afs.mcp_admission (uncached; wrap it in mcpgateway's cache).
func (g *McpGatewayStore) Snapshot(ctx context.Context, c mcpgateway.Caller) (*mcpgateway.Snapshot, error) {
	if c.SubjectKind != "agent" && c.SubjectKind != "pool" {
		return nil, errors.New("postgres: MCP caller is neither an agent nor a pool")
	}
	rows, err := g.pool.Query(ctx, mcpSnapshotSQL, c.SubjectKind, c.SubjectID)
	if err != nil {
		return nil, fmt.Errorf("read mcp admission: %w", err)
	}
	defer rows.Close()
	snap := &mcpgateway.Snapshot{Mode: "search"}
	servers := map[string]int{}
	h := sha256.New()
	for rows.Next() {
		var id, slug, kind, teamMode, pkind, name, digest, reason, mode string
		var allowed bool
		var rate int
		var def, limits []byte
		if err := rows.Scan(&id, &slug, &kind, &teamMode, &pkind, &name, &digest, &allowed, &reason, &rate, &def, &limits, &mode); err != nil {
			return nil, fmt.Errorf("read mcp admission: %w", err)
		}
		snap.Mode = mode
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\x00%s\x00%d\x00%s\n", id, pkind, name, allowed, digest, rate, teamMode)
		if !serverLevel[reason] {
			if _, ok := servers[id]; !ok {
				servers[id] = len(snap.Servers)
				snap.Servers = append(snap.Servers, mcpgateway.Server{ID: id, Slug: slug, Kind: kind, ReadOnly: teamMode == "read_only", Limits: limitsOf(limits)})
			}
		}
		p, ok := mcpinventory.Describe(slug, mcp.Kind(pkind), def)
		if !ok {
			continue
		}
		p.Status = mcp.StatusApproved
		snap.Entries = append(snap.Entries, mcpgateway.Entry{ServerID: id, Primitive: p, Definition: def, Digest: digest,
			Decision: mcp.Decision{Allowed: allowed, Reason: mcp.Reason(reason), RateLimit: rate}, Reason: mcp.Reason(reason)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mcp admission: %w", err)
	}
	_, _ = fmt.Fprintf(h, "mode=%s", snap.Mode)
	snap.Version = hex.EncodeToString(h.Sum(nil))[:16]
	return snap, nil
}

const mcpTargetCols = `s.id, coalesce(s.team_id, ''), s.slug, s.transport, s.location, coalesce(s.url, ''), coalesce(s.ca_bundle, ''),
  s.auth_kind, coalesce(s.auth_header, ''), s.federation, coalesce(s.protocol_version, ''), s.credential_mode, s.review_status,
  s.drift_policy`

func scanTarget(row pgx.Row) (mcpup.Target, error) {
	var t mcpup.Target
	var transport, location, auth string
	var fed []byte
	if err := row.Scan(&t.ID, &t.TeamID, &t.Slug, &transport, &location, &t.URL, &t.CABundle, &auth, &t.AuthHeader, &fed,
		&t.ProtocolVersion, &t.CredentialMode, &t.ReviewStatus, &t.DriftPolicy); err != nil {
		return mcpup.Target{}, err
	}
	t.Transport, t.Location, t.AuthKind = domain.McpTransport(transport), domain.EndpointLocation(location), domain.McpAuthKind(auth)
	if len(fed) > 0 {
		t.Federation = &domain.McpFederation{}
		if err := json.Unmarshal(fed, t.Federation); err != nil {
			return mcpup.Target{}, fmt.Errorf("server %s federation: %w", t.ID, err)
		}
	}
	return t, nil
}

// Target implements mcpup.Targets.
func (g *McpGatewayStore) Target(ctx context.Context, id string) (mcpup.Target, error) {
	t, err := scanTarget(g.pool.QueryRow(ctx, `SELECT `+mcpTargetCols+` FROM afs.mcp_servers s WHERE s.id = $1`, id))
	if err != nil {
		return mcpup.Target{}, notFound(fmt.Errorf("read mcp server: %w", err))
	}
	return t, nil
}

// MarkMcpCredential implements mcpup.State: a change of status emits auth_expired or auth_restored in the same
// transaction. A credential that is not there, or already in that status, changes nothing.
func (g *McpGatewayStore) MarkMcpCredential(ctx context.Context, serverID, scope, status string) error {
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var team *string
		err := tx.QueryRow(ctx, `UPDATE afs.mcp_credentials SET status = $3, expires_at = CASE WHEN $3 = 'expired' THEN now() ELSE expires_at END
WHERE server_id = $1 AND scope = $2 AND status <> $3 AND status <> 'missing' RETURNING team_id`, serverID, scope, status).Scan(&team)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mark mcp credential: %w", err)
		}
		teamID := ""
		if team != nil {
			teamID = *team
		}
		return insertGatewayEvent(ctx, tx, domain.McpAuthEvent(status == "expired"), "mcp_servers", serverID, teamID,
			map[string]any{"serverId": serverID, "teamId": teamID})
	})
}

// ReportMcpFederation implements mcpup.State (afs_gateway may update status, last_error and checked_at).
func (g *McpGatewayStore) ReportMcpFederation(ctx context.Context, serverID, teamID, status, lastError string) error {
	_, err := g.pool.Exec(ctx, `UPDATE afs.mcp_federation_states SET status = $3, last_error = nullif($4, ''), checked_at = now()
WHERE server_id = $1 AND team_id = $2 AND (status IS DISTINCT FROM $3 OR last_error IS DISTINCT FROM nullif($4, ''))`, serverID, teamID, status, lastError)
	if err != nil {
		return fmt.Errorf("report mcp federation: %w", err)
	}
	return nil
}

// insertGatewayEvent writes an outbox event the gateway emits (API contract §7), actor system:afs-gateway.
func insertGatewayEvent(ctx context.Context, tx pgx.Tx, t domain.EventType, kind, aggregate, teamID string, data map[string]any) error {
	id, err := domain.NewID("evt", time.Now(), rand.Reader)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(domain.EventDetail{ID: string(id), OccurredAt: time.Now().UTC().Truncate(time.Second), TeamID: teamID,
		Actor: "system:afs-gateway", Data: data})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertOutboxSQL, string(t), kind, aggregate, teamID, payload); err != nil {
		return fmt.Errorf("outbox %s: %w", t, err)
	}
	return nil
}

// McpRefreshTargets lists the servers this gateway may refresh: the team's own and the enterprise servers it
// switched on, in review or in service.
func (g *McpGatewayStore) McpRefreshTargets(ctx context.Context, teamID string) ([]mcpup.RefreshTarget, error) {
	rows, err := g.pool.Query(ctx, `SELECT `+mcpTargetCols+`, s.refresh_requested_at IS NOT NULL,
  coalesce((SELECT max(p.last_seen) FROM afs.mcp_primitives p WHERE p.server_id = s.id), 'epoch')
FROM afs.mcp_servers s
WHERE s.review_status IN ('pending_review', 'approved', 'suspended')
  AND (s.team_id = $1 OR (s.team_id IS NULL AND EXISTS (SELECT 1 FROM afs.team_mcp_servers x
       WHERE x.team_id = $1 AND x.server_id = s.id AND x.enabled)))
ORDER BY s.slug`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read mcp refresh targets: %w", err)
	}
	defer rows.Close()
	var out []mcpup.RefreshTarget
	for rows.Next() {
		var r mcpup.RefreshTarget
		var transport, location, auth string
		var fed []byte
		if err := rows.Scan(&r.ID, &r.TeamID, &r.Slug, &transport, &location, &r.URL, &r.CABundle, &auth, &r.AuthHeader, &fed,
			&r.ProtocolVersion, &r.CredentialMode, &r.ReviewStatus, &r.DriftPolicy, &r.Requested, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("read mcp refresh targets: %w", err)
		}
		r.Transport, r.Location, r.AuthKind = domain.McpTransport(transport), domain.EndpointLocation(location), domain.McpAuthKind(auth)
		if len(fed) > 0 {
			r.Federation = &domain.McpFederation{}
			_ = json.Unmarshal(fed, r.Federation)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// McpRefreshInterval reads mcp.refresh_interval_minutes.
func (g *McpGatewayStore) McpRefreshInterval(ctx context.Context) (time.Duration, error) {
	var m int
	if err := g.pool.QueryRow(ctx, `SELECT coalesce((afs.mcp_setting('mcp.refresh_interval_minutes'))::int, 15)`).Scan(&m); err != nil {
		return 0, fmt.Errorf("read mcp.refresh_interval_minutes: %w", err)
	}
	return time.Duration(m) * time.Minute, nil
}

// McpStored reads the server's primitives for mcpinventory.Build.
func (g *McpGatewayStore) McpStored(ctx context.Context, serverID string) ([]mcpinventory.Stored, error) {
	rows, err := g.pool.Query(ctx, `SELECT id, kind, name, CASE WHEN approved_digest IS NOT NULL THEN definition END, digest,
  coalesce(approved_digest, ''), status, CASE WHEN approved_digest IS NOT NULL THEN annotations END, removed_at IS NOT NULL
FROM afs.mcp_primitives WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("read mcp primitives: %w", err)
	}
	defer rows.Close()
	var out []mcpinventory.Stored
	for rows.Next() {
		var s mcpinventory.Stored
		var kind, status string
		var def, ann []byte
		if err := rows.Scan(&s.ID, &kind, &s.Name, &def, &s.Digest, &s.ApprovedDigest, &status, &ann, &s.Removed); err != nil {
			return nil, fmt.Errorf("read mcp primitives: %w", err)
		}
		s.Kind, s.Status = mcp.Kind(kind), mcp.Status(status)
		if def != nil {
			s.Definition = def
		}
		if ann != nil {
			s.Annotations = &mcp.Annotations{}
			_ = json.Unmarshal(ann, s.Annotations)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// summary is the first sentence of a description, at most 200 characters.
func summary(d string) string {
	d = strings.TrimSpace(d)
	if i := strings.IndexAny(d, ".\n"); i >= 0 {
		d = d[:i+1]
	}
	return clip(strings.TrimSpace(d), 200)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func annotationsJSON(a mcp.Annotations) []byte {
	b, _ := json.Marshal(a)
	return b
}

// ErrMcpRefreshBusy means another replica holds the server's refresh.
var ErrMcpRefreshBusy = mcpup.ErrBusy

// ApplyMcpPlan stores a refresh in one transaction under the server's lease (an advisory transaction lock, so
// only one replica of any team gateway applies a refresh at a time): inserts are held, changes are held or
// approved automatically as the plan says, removals are marked, and at most one primitive_changed event is
// emitted. The refresh request is cleared and the negotiated protocol version recorded.
func (g *McpGatewayStore) ApplyMcpPlan(ctx context.Context, t mcpup.Target, plan mcpinventory.Plan, protocolVersion string) error {
	return pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('afs.mcp_refresh'), hashtext($1))`, t.ID).Scan(&got); err != nil {
			return fmt.Errorf("mcp refresh lease: %w", err)
		}
		if !got {
			return ErrMcpRefreshBusy
		}
		for _, it := range plan.Items {
			if err := applyItem(ctx, tx, t.ID, it); err != nil {
				return err
			}
		}
		if len(plan.Events) > 0 {
			it := plan.Events[0]
			change := "changed"
			if it.Change == mcp.ChangeNew {
				change = "new"
			}
			var pid string
			if err := tx.QueryRow(ctx, `SELECT id FROM afs.mcp_primitives WHERE server_id = $1 AND kind = $2 AND name = $3`, t.ID, string(it.Primitive.Kind), it.Primitive.Name).Scan(&pid); err != nil {
				return fmt.Errorf("read mcp primitive: %w", err)
			}
			data := map[string]any{"serverId": t.ID, "primitiveId": pid, "kind": string(it.Primitive.Kind), "name": it.Primitive.Name,
				"change": change, "digest": it.Digest, "autoApproved": it.Action == mcpinventory.ActionAutoApprove, "count": len(plan.Events)}
			if t.TeamID != "" {
				data["teamId"] = t.TeamID
			}
			if err := insertGatewayEvent(ctx, tx, domain.EventMcpServerPrimitiveChanged, "mcp_servers", t.ID, t.TeamID, data); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE afs.mcp_servers SET refresh_requested_at = NULL,
  protocol_version = coalesce(nullif($2, ''), protocol_version) WHERE id = $1`, t.ID, protocolVersion)
		if err != nil {
			return fmt.Errorf("record mcp refresh: %w", err)
		}
		return nil
	})
}

func newPrimitiveID() (string, error) {
	id, err := domain.NewID(domain.PrefixMcpPrimitive, time.Now(), rand.Reader)
	return string(id), err
}

func applyItem(ctx context.Context, tx pgx.Tx, serverID string, it mcpinventory.Item) error {
	p := it.Primitive
	var err error
	switch it.Action {
	case mcpinventory.ActionUnchanged:
		_, err = tx.Exec(ctx, `UPDATE afs.mcp_primitives SET last_seen = now() WHERE id = $1`, it.StoredID)
	case mcpinventory.ActionInsertHeld:
		var id string
		if id, err = newPrimitiveID(); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO afs.mcp_primitives (id, server_id, kind, name, title, summary, annotations, definition, digest, change, status)
VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7, $8, $9, 'new', 'pending_review')`,
			id, serverID, string(p.Kind), p.Name, clip(p.Title, 200), summary(p.Description), annotationsJSON(p.Annotations), []byte(it.Definition), it.Digest)
	case mcpinventory.ActionHold, mcpinventory.ActionAutoApprove:
		change := "incompatible"
		if it.Change == mcp.ChangeCompatible {
			change = "compatible"
		}
		_, err = tx.Exec(ctx, `UPDATE afs.mcp_primitives SET digest = $2, pending_definition = $3, change = $4, status = 'pending_review', last_seen = now()
WHERE id = $1`, it.StoredID, it.Digest, []byte(it.Definition), change)
		if err == nil && it.Action == mcpinventory.ActionAutoApprove {
			_, err = tx.Exec(ctx, `UPDATE afs.mcp_primitives SET status = 'approved', title = nullif($2, ''), summary = nullif($3, '') WHERE id = $1`,
				it.StoredID, clip(p.Title, 200), summary(p.Description))
		}
	case mcpinventory.ActionRemove:
		_, err = tx.Exec(ctx, `UPDATE afs.mcp_primitives SET removed_at = now() WHERE id = $1 AND removed_at IS NULL`, it.StoredID)
	case mcpinventory.ActionReappear:
		_, err = tx.Exec(ctx, `UPDATE afs.mcp_primitives SET removed_at = NULL, last_seen = now() WHERE id = $1`, it.StoredID)
	}
	if err != nil {
		return fmt.Errorf("apply mcp %s %s: %w", it.Action, p.Name, err)
	}
	return nil
}
