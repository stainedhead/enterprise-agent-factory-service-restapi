package mcpcatalog_test

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// repo is an in-memory mcpcatalog.Repository with the database's rules the use cases rely on.
type repo struct {
	settings    mcpcatalog.Settings
	servers     map[string]domain.McpServerRecord
	prims       map[string]domain.McpPrimitiveRecord
	reviews     []domain.McpReview
	rules       map[string][]domain.McpPolicyRule
	teams       map[string]mcpcatalog.TeamInfo
	switches    map[string]domain.TeamMcpSwitch
	creds       map[string]domain.McpCredentialRecord
	fedTests    map[string]bool
	refreshed   map[string]bool
	subjects    map[string]domain.McpSubject
	admission   []domain.McpAdmissionRow
	events      []domain.McpEventRecord
	usage       []domain.McpUsageRow
	captures    []domain.McpQueryCapture
	oauth       map[string]oauthRow
	federated   map[string]domain.FederatedIdentityRecord
	adoption    mcpcatalog.Adoption
	lastFilter  domain.McpEventFilter
	lastService bool
	lastUsage   domain.McpUsageQuery
}

func newRepo() *repo {
	return &repo{
		settings: mcpcatalog.Settings{Approval: "team_all", CaptureMaxDays: 7, DefaultAgentMode: domain.McpAgentModeSearch,
			AllowedRegistries: []string{"registry.example.com"}, HostDenyList: []string{"*.denied.example"},
			AllowedAudiences: []string{"api://tools", "https://*.example.com"}},
		servers: map[string]domain.McpServerRecord{}, prims: map[string]domain.McpPrimitiveRecord{}, rules: map[string][]domain.McpPolicyRule{},
		teams: map[string]mcpcatalog.TeamInfo{team: {ID: team, Slug: "alpha", Namespace: "afs-t-alpha", MayAddMcpServers: true, GatewayDeployment: "per_namespace"},
			team2: {ID: team2, Slug: "beta", Namespace: "afs-t-beta", GatewayDeployment: "shared"}},
		switches: map[string]domain.TeamMcpSwitch{}, creds: map[string]domain.McpCredentialRecord{}, fedTests: map[string]bool{},
		refreshed: map[string]bool{}, subjects: map[string]domain.McpSubject{
			"agent/" + agentID: {Kind: "agent", TeamID: team, ID: agentID, Access: "all"},
			"pool/" + poolID:   {Kind: "pool", TeamID: team, ID: poolID, Access: "selected", Mode: domain.McpAgentModeFull},
		}, federated: map[string]domain.FederatedIdentityRecord{}, oauth: map[string]oauthRow{},
	}
}

func (r *repo) Settings(context.Context, port.Tx) (mcpcatalog.Settings, error) {
	return r.settings, nil
}

func (r *repo) ListServers(_ context.Context, _ port.Tx, f mcpcatalog.ServerFilter, after string, limit int) ([]domain.McpServerRecord, error) {
	var out []domain.McpServerRecord
	for _, x := range r.servers {
		if x.ID <= after || (f.Scope == "enterprise" && x.IsTeam()) || (f.Scope == "team" && !x.IsTeam()) ||
			(f.ReviewStatus != "" && string(x.ReviewStatus) != f.ReviewStatus) || (f.TeamID != "" && x.TeamID != f.TeamID) {
			continue
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetServer(_ context.Context, _ port.Tx, id string, _ bool) (domain.McpServerRecord, error) {
	x, ok := r.servers[id]
	if !ok {
		return x, domain.NotFound("mcp server")
	}
	return x, nil
}

func (r *repo) InsertServer(_ context.Context, _ port.Tx, s domain.McpServerRecord) error {
	for _, x := range r.servers {
		if x.Slug == s.Slug && (x.TeamID == "" || s.TeamID == "" || x.TeamID == s.TeamID) {
			return domain.Conflict(domain.ReasonSlugTaken, "taken", nil)
		}
	}
	s.Version, s.CreatedAt, s.UpdatedAt, s.CreatedBy = 1, time.Unix(0, 0), time.Unix(0, 0), "usr_x"
	r.servers[s.ID] = s
	return nil
}

func (r *repo) UpdateServer(_ context.Context, _ port.Tx, s domain.McpServerRecord, expect domain.Version) error {
	cur, ok := r.servers[s.ID]
	if !ok {
		return domain.NotFound("mcp server")
	}
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	if !domain.ReviewTransitionAllowed(cur.ReviewStatus, s.ReviewStatus, cur.SuspendedFrom) {
		return domain.Conflict(domain.ReasonInvalidReviewTransition, "db", nil)
	}
	if s.ReviewStatus != domain.McpSuspended {
		s.SuspendedFrom = ""
	}
	s.Version = expect + 1
	r.servers[s.ID] = s
	return nil
}

func (r *repo) DeleteServer(_ context.Context, _ port.Tx, id string, expect domain.Version) error {
	if r.servers[id].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.servers, id)
	return nil
}

func (r *repo) ServerInUse(_ context.Context, _ port.Tx, id string) (bool, error) {
	for _, sw := range r.switches {
		if sw.ServerID == id && sw.Enabled {
			return true, nil
		}
	}
	return false, nil
}

func (r *repo) Adoption(context.Context, port.Tx, string) (mcpcatalog.Adoption, error) {
	return r.adoption, nil
}

func (r *repo) RequestRefresh(_ context.Context, _ port.Tx, id string) error {
	r.refreshed[id] = true
	return nil
}

func (r *repo) ListPrimitives(_ context.Context, _ port.Tx, serverID string, f mcpcatalog.PrimitiveFilter, after string, limit int) ([]domain.McpPrimitiveRecord, error) {
	var out []domain.McpPrimitiveRecord
	for _, p := range r.prims {
		if p.ServerID == serverID && p.ID > after && (f.Kind == "" || f.Kind == p.Kind) && (f.Status == "" || f.Status == p.Status) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetPrimitive(_ context.Context, _ port.Tx, serverID, id string, _ bool) (domain.McpPrimitiveRecord, error) {
	p, ok := r.prims[id]
	if !ok || p.ServerID != serverID {
		return p, domain.NotFound("primitive")
	}
	return p, nil
}

func (r *repo) DecidePrimitive(_ context.Context, _ port.Tx, id, status string) error {
	p := r.prims[id]
	p.Status = status
	if status == domain.McpPrimitiveApproved {
		if len(p.PendingDefinition) > 0 {
			p.Definition = p.PendingDefinition
		}
		p.PendingDefinition, p.ApprovedDigest, p.Change = nil, p.Digest, ""
	}
	r.prims[id] = p
	return nil
}

func (r *repo) InsertReview(_ context.Context, _ port.Tx, rv domain.McpReview) error {
	r.reviews = append(r.reviews, rv)
	return nil
}

func (r *repo) ReviewQueue(context.Context, port.Tx) ([]domain.McpReviewQueueItem, error) {
	var out []domain.McpReviewQueueItem
	ids := make([]string, 0, len(r.servers))
	for id := range r.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		x := r.servers[id]
		if x.ReviewStatus == domain.McpPendingReview {
			out = append(out, domain.McpReviewQueueItem{Kind: "server", ServerID: x.ID, TeamID: x.TeamID, Slug: x.Slug, RiskTier: x.RiskTier, Approver: "security"})
		}
	}
	return out, nil
}

func scopeKey(s mcpcatalog.PolicyScope) string {
	return s.ServerID + "/" + s.Level + "/" + s.TeamID + "/" + s.SubjectID
}

func (r *repo) ListRules(_ context.Context, _ port.Tx, s mcpcatalog.PolicyScope) ([]domain.McpPolicyRule, error) {
	return r.rules[scopeKey(s)], nil
}

func (r *repo) ReplaceRules(_ context.Context, _ port.Tx, s mcpcatalog.PolicyScope, rules []domain.McpPolicyRule, _ []string) error {
	r.rules[scopeKey(s)] = rules
	return nil
}

func (r *repo) TeamInfo(_ context.Context, _ port.Tx, teamID string) (mcpcatalog.TeamInfo, error) {
	t, ok := r.teams[teamID]
	if !ok {
		return t, domain.NotFound("team")
	}
	return t, nil
}

func (r *repo) row(teamID string, x domain.McpServerRecord) (domain.TeamMcpServerRow, bool) {
	sw, has := r.switches[teamID+"/"+x.ID]
	if x.TeamID != teamID && (x.TeamID != "" || (!x.Published && !has)) {
		return domain.TeamMcpServerRow{}, false
	}
	row := domain.TeamMcpServerRow{Server: x}
	if has {
		row.Switch = &sw
	}
	if c, ok := r.creds[x.ID+"/"+x.CredentialScope(teamID)]; ok {
		row.Credential = &c
	}
	if r.fedTests[x.ID+"/"+teamID] {
		row.FederationState = "unknown"
	}
	return row, true
}

func (r *repo) ListTeamServers(_ context.Context, _ port.Tx, teamID string) ([]domain.TeamMcpServerRow, error) {
	var out []domain.TeamMcpServerRow
	for _, x := range r.servers {
		if row, ok := r.row(teamID, x); ok {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server.ID < out[j].Server.ID })
	return out, nil
}

func (r *repo) GetTeamServer(_ context.Context, _ port.Tx, teamID, serverID string) (domain.TeamMcpServerRow, error) {
	x, ok := r.servers[serverID]
	if !ok {
		return domain.TeamMcpServerRow{}, domain.NotFound("mcp server")
	}
	row, ok := r.row(teamID, x)
	if !ok {
		return row, domain.NotFound("mcp server")
	}
	return row, nil
}

func (r *repo) PutSwitch(_ context.Context, _ port.Tx, sw domain.TeamMcpSwitch) error {
	k := sw.TeamID + "/" + sw.ServerID
	sw.Version = r.switches[k].Version + 1
	r.switches[k] = sw
	return nil
}

func (r *repo) TeamEnabledServers(_ context.Context, _ port.Tx, teamID string) ([]string, error) {
	var out []string
	for _, sw := range r.switches {
		if sw.TeamID == teamID && sw.Enabled {
			out = append(out, sw.ServerID)
		}
	}
	return out, nil
}

func (r *repo) GetCredential(_ context.Context, _ port.Tx, serverID, scope string) (*domain.McpCredentialRecord, error) {
	c, ok := r.creds[serverID+"/"+scope]
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func (r *repo) CredentialPath(_ context.Context, _ port.Tx, teamID, slug string) (string, error) {
	if teamID == "" {
		return "afs/_service/gateway/mcp/" + slug, nil
	}
	return "afs/" + r.teams[teamID].Namespace + "/_gateway/mcp/" + slug, nil
}

func (r *repo) UpsertCredential(_ context.Context, _ port.Tx, c domain.McpCredentialRecord) error {
	k := c.ServerID + "/" + c.Scope
	c.Version = r.creds[k].Version + 1
	r.creds[k] = c
	return nil
}

func (r *repo) DeleteCredential(_ context.Context, _ port.Tx, serverID, scope string) error {
	delete(r.creds, serverID+"/"+scope)
	return nil
}

func (r *repo) RequestFederationTest(_ context.Context, _ port.Tx, serverID, teamID string) error {
	r.fedTests[serverID+"/"+teamID] = true
	return nil
}

func (r *repo) Subject(_ context.Context, _ port.Tx, teamID, kind, id string) (domain.McpSubject, error) {
	s, ok := r.subjects[kind+"/"+id]
	if !ok || s.TeamID != teamID {
		return s, domain.NotFound(kind)
	}
	return s, nil
}

func (r *repo) SubjectAccount(ctx context.Context, tx port.Tx, teamID, kind, id string) (string, string, error) {
	if _, err := r.Subject(ctx, tx, teamID, kind, id); err != nil {
		return "", "", err
	}
	return r.teams[teamID].Namespace, kind + "-" + strings.TrimPrefix(id, "agt_"), nil
}

func (r *repo) Admission(context.Context, port.Tx, string, string) ([]domain.McpAdmissionRow, error) {
	return r.admission, nil
}

func (r *repo) ListEvents(_ context.Context, _ port.Tx, f domain.McpEventFilter, service bool, _ string, limit int) ([]domain.McpEventRecord, error) {
	r.lastFilter, r.lastService = f, service
	out := r.events
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) UsageReport(_ context.Context, _ port.Tx, q domain.McpUsageQuery) ([]domain.McpUsageRow, error) {
	r.lastUsage = q
	return r.usage, nil
}

func (r *repo) OpenCapture(context.Context, port.Tx) (*domain.McpQueryCapture, error) {
	for i := range r.captures {
		c := r.captures[i]
		if c.StoppedAt == nil && c.Until.After(now) {
			return &c, nil
		}
	}
	return nil, nil
}

func (r *repo) LatestCapture(context.Context, port.Tx) (*domain.McpQueryCapture, error) {
	for i := len(r.captures) - 1; i >= 0; i-- {
		if r.captures[i].StoppedAt == nil {
			c := r.captures[i]
			return &c, nil
		}
	}
	return nil, nil
}

func (r *repo) InsertCapture(_ context.Context, _ port.Tx, c domain.McpQueryCapture) error {
	r.captures = append(r.captures, c)
	return nil
}

func (r *repo) StopCapture(_ context.Context, _ port.Tx, id string, at time.Time, by string) error {
	for i := range r.captures {
		if r.captures[i].ID == id {
			r.captures[i].StoppedAt, r.captures[i].StoppedBy = &at, by
		}
	}
	return nil
}

func fedKey(kind, teamID, subjectID, name string) string {
	return kind + "/" + teamID + "/" + subjectID + "/" + name
}

func (r *repo) ListFederated(_ context.Context, _ port.Tx, kind, teamID, subjectID string) ([]domain.FederatedIdentityRecord, error) {
	var out []domain.FederatedIdentityRecord
	for k, f := range r.federated {
		if strings.HasPrefix(k, fedKey(kind, teamID, subjectID, "")) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *repo) GetFederated(_ context.Context, _ port.Tx, kind, teamID, subjectID, name string) (*domain.FederatedIdentityRecord, error) {
	f, ok := r.federated[fedKey(kind, teamID, subjectID, name)]
	if !ok {
		return nil, nil
	}
	return &f, nil
}

func (r *repo) InsertFederated(_ context.Context, _ port.Tx, kind string, f domain.FederatedIdentityRecord) error {
	f.Version = 1
	r.federated[fedKey(kind, f.TeamID, f.SubjectID, f.Name)] = f
	return nil
}

func (r *repo) UpdateFederated(_ context.Context, _ port.Tx, kind string, f domain.FederatedIdentityRecord, expect domain.Version) error {
	f.Version = expect + 1
	r.federated[fedKey(kind, f.TeamID, f.SubjectID, f.Name)] = f
	return nil
}

func (r *repo) DeleteFederated(_ context.Context, _ port.Tx, kind, teamID, subjectID, name string, expect domain.Version) error {
	k := fedKey(kind, teamID, subjectID, name)
	if r.federated[k].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.federated, k)
	return nil
}

// secrets is a fake SecretWriter that hands out ARNs for the path.
type secrets struct {
	created, deleted, scheduled []string
	values                      map[string]string
	fail                        bool
}

func (s *secrets) Create(_ context.Context, name string, v domain.SecretValue, _ string, _ map[string]string) (string, error) {
	if s.fail {
		return "", domain.Unavailable("secrets manager", nil)
	}
	arn := "arn:aws:secretsmanager:us-east-1:000000000000:secret:" + name + "-AbCdEf"
	s.created = append(s.created, arn)
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[arn] = v.Expose()
	return arn, nil
}

func (s *secrets) PutValue(_ context.Context, arn string, v domain.SecretValue, _ string) error {
	if s.fail {
		return domain.Unavailable("secrets manager", nil)
	}
	s.values[arn] = v.Expose()
	return nil
}

func (s *secrets) ForceDelete(_ context.Context, arn string) error {
	s.deleted = append(s.deleted, arn)
	return nil
}

func (s *secrets) ScheduleDeletion(_ context.Context, arn string, _ int) error {
	s.scheduled = append(s.scheduled, arn)
	return nil
}

// oauthRow is a stored connect state; the fake records every state as started by usr_a (teamAdmin).
type oauthRow struct {
	st      mcpcatalog.OAuthState
	expires time.Time
	used    bool
}

func (r *repo) InsertOAuthState(_ context.Context, _ port.Tx, hash []byte, teamID, serverID string, expiresAt time.Time) error {
	r.oauth[string(hash)] = oauthRow{st: mcpcatalog.OAuthState{TeamID: teamID, ServerID: serverID, CreatedBy: "usr_a"}, expires: expiresAt}
	return nil
}

func (r *repo) ClaimOAuthState(_ context.Context, _ port.Tx, hash []byte) (*mcpcatalog.OAuthState, error) {
	row, ok := r.oauth[string(hash)]
	if !ok || row.used || !row.expires.After(now) {
		return nil, nil
	}
	row.used = true
	r.oauth[string(hash)] = row
	return &row.st, nil
}
