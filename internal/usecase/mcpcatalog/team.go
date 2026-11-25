package mcpcatalog

import (
	"context"
	"slices"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// TeamServerView is a server as one team sees it, with the computed fields.
type TeamServerView struct {
	Row             domain.TeamMcpServerRow
	CredentialState string
	Approver        string
}

// Mode is the team's mode for the server (full when it has no switch).
func (v TeamServerView) Mode() domain.McpTeamMode {
	if v.Row.Switch != nil {
		return v.Row.Switch.Mode
	}
	return domain.McpTeamModeFull
}

// Managed is enterprise or team.
func (v TeamServerView) Managed() string { return v.Row.Server.Scope() }

func teamView(st Settings, r domain.TeamMcpServerRow) TeamServerView {
	v := TeamServerView{Row: r, CredentialState: domain.McpCredentialStateOf(r.Server.AuthKind, r.Credential)}
	if r.Server.ReviewStatus == domain.McpPendingReview {
		v.Approver = approverFor(st, r.Server)
	}
	return v
}

// teamServerOf scopes a team path to the team's own servers: an enterprise id is 403 not_team_server and another
// team's server is not found.
func (s *Service) teamServerOf(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) func(domain.McpServerRecord) error {
	return func(x domain.McpServerRecord) error {
		switch x.TeamID {
		case teamID:
			return nil
		case "":
			return s.refuse(ctx, c, action, teamID, perm, domain.ReasonNotTeamServer, "an enterprise server changes through the service catalog")
		}
		return domain.NotFound("mcp server")
	}
}

// ListTeam implements ListTeamMcpServers: published enterprise servers with the team's switch, and its own servers.
func (s *Service) ListTeam(ctx context.Context, c acting.Caller, teamID, state string, p acting.Page) ([]TeamServerView, string, error) {
	if err := s.checkTeam(ctx, c, "ListTeamMcpServers", teamID, authz.TeamMcpServersRead); err != nil {
		return nil, "", err
	}
	if state != "" && !slices.Contains([]string{"on", "off", "pending"}, state) {
		return nil, "", domain.Validation(domain.FieldError{Field: "state", Reason: "must be on, off or pending"})
	}
	p = p.Normalized()
	var out []TeamServerView
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := s.Repo.ListTeamServers(ctx, tx, teamID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Server.ID > p.Cursor && (state == "" || r.State() == state) {
				out = append(out, teamView(st, r))
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(out, p.Limit, func(v TeamServerView) string { return v.Row.Server.ID })
	if page == nil {
		page = []TeamServerView{}
	}
	return page, next, nil
}

// GetTeam implements GetTeamMcpServer.
func (s *Service) GetTeam(ctx context.Context, c acting.Caller, teamID, serverID string) (TeamServerView, error) {
	if err := s.checkTeam(ctx, c, "GetTeamMcpServer", teamID, authz.TeamMcpServersRead); err != nil {
		return TeamServerView{}, err
	}
	var out TeamServerView
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		r, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		out = teamView(st, r)
		return nil
	})
	return out, err
}

// SwitchInput is a PutTeamMcpServer body.
type SwitchInput struct {
	Enabled bool
	Mode    domain.McpTeamMode
}

// PutSwitch implements PutTeamMcpServer: switch a server on or off for the team, full or read-only. An enterprise
// server must be published and a team server approved; the team needs its own gateway (CD-44).
func (s *Service) PutSwitch(ctx context.Context, c acting.Caller, teamID, serverID string, in SwitchInput, expect *domain.Version) (TeamServerView, error) {
	if err := s.checkTeam(ctx, c, "PutTeamMcpServer", teamID, authz.TeamMcpServersWrite); err != nil {
		return TeamServerView{}, err
	}
	if in.Mode == "" {
		in.Mode = domain.McpTeamModeFull
	}
	if in.Mode != domain.McpTeamModeFull && in.Mode != domain.McpTeamModeReadOnly {
		return TeamServerView{}, domain.Validation(domain.FieldError{Field: "mode", Reason: "must be full or read_only"})
	}
	var out TeamServerView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		if expect != nil && cur.Switch != nil {
			if err := domain.CheckVersion(cur.Switch.Version, *expect); err != nil {
				return err
			}
		}
		srv := cur.Server
		if in.Enabled {
			switch {
			case !srv.IsTeam() && !srv.Published:
				return domain.Conflict(domain.ReasonServerNotPublished, "the server is not published in the catalog", nil)
			case srv.IsTeam() && srv.ReviewStatus != domain.McpApproved:
				return domain.Conflict(domain.ReasonServerNotApproved, "the server is "+string(srv.ReviewStatus)+"; it must be approved first", nil)
			}
			team, err := s.Repo.TeamInfo(ctx, tx, teamID)
			if err != nil {
				return err
			}
			if team.GatewayDeployment != "per_namespace" {
				return domain.Conflict(domain.ReasonTeamGatewayRequired, "the team needs its own gateway before it can switch on MCP servers", nil)
			}
		}
		if err := s.Repo.PutSwitch(ctx, tx, domain.TeamMcpSwitch{TeamID: teamID, ServerID: serverID, Enabled: in.Enabled, Mode: in.Mode}); err != nil {
			return err
		}
		next, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		audit := func(r domain.TeamMcpServerRow) any {
			if r.Switch == nil {
				return nil
			}
			return map[string]any{"teamId": teamID, "serverId": serverID, "enabled": r.Switch.Enabled, "mode": r.Switch.Mode}
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamMcpServer", kindSwitch, teamID+"/"+serverID, teamID, audit(cur), audit(next))); err != nil {
			return err
		}
		was := cur.Switch != nil && cur.Switch.Enabled
		modeChanged := cur.Switch == nil || cur.Switch.Mode != in.Mode
		if was != in.Enabled || (in.Enabled && modeChanged) {
			if err := s.Emit(ctx, tx, c, domain.TeamMcpSwitchEvent(in.Enabled), teamID, kindSwitch, teamID+"/"+serverID,
				map[string]any{"teamId": teamID, "serverId": serverID, "mode": string(in.Mode)}); err != nil {
				return err
			}
		}
		out = teamView(st, next)
		return nil
	})
	return out, err
}

// CreateTeam implements CreateTeamMcpServer: needs the team's mayAddMcpServers (403 team_servers_not_allowed).
func (s *Service) CreateTeam(ctx context.Context, c acting.Caller, teamID string, in ServerInput, idem *acting.Idem) (Created, error) {
	const action = "CreateTeamMcpServer"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return Created{}, err
	}
	in.CredentialMode, in.DriftPolicy, in.DefaultForNewTeams, in.AllowSampling, in.TrustAnnotations = nil, nil, nil, nil, nil
	return s.create(ctx, c, action, teamID, in, idem, func(ctx context.Context, tx port.Tx) error {
		team, err := s.Repo.TeamInfo(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !team.MayAddMcpServers {
			return s.refuse(ctx, c, action, teamID, authz.TeamMcpServersWrite, domain.ReasonTeamServersNotAllowed,
				"a service admin has not allowed this team to add MCP servers")
		}
		return nil
	})
}

// UpdateTeam implements UpdateTeamMcpServer (team servers only).
func (s *Service) UpdateTeam(ctx context.Context, c acting.Caller, teamID, serverID string, in ServerInput, expect domain.Version) (ServerView, error) {
	const action = "UpdateTeamMcpServer"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return ServerView{}, err
	}
	in.Slug, in.CredentialMode, in.DriftPolicy, in.DefaultForNewTeams, in.AllowSampling, in.TrustAnnotations = nil, nil, nil, nil, nil, nil
	return s.update(ctx, c, action, serverID, in, expect, s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersWrite))
}

// DeleteTeam implements DeleteTeamMcpServer (team servers only; refused while switched on).
func (s *Service) DeleteTeam(ctx context.Context, c acting.Caller, teamID, serverID string, expect domain.Version) error {
	const action = "DeleteTeamMcpServer"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return err
	}
	return s.remove(ctx, c, action, serverID, expect, s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersWrite))
}

// runChecks computes the checks the API can decide from stored state; the gateway adds reachability, tls,
// protocol_version and inventory when it refreshes.
func runChecks(st Settings, x domain.McpServerRecord, prims []domain.McpPrimitiveRecord, at time.Time) []domain.McpCheck {
	check := func(name, outcome, detail string) domain.McpCheck {
		return domain.McpCheck{Name: name, Outcome: outcome, Detail: detail, CheckedAt: at}
	}
	var out []domain.McpCheck
	if h := x.Host(); h != "" {
		if mcp.HostDenied(h, st.HostDenyList) {
			out = append(out, check("host_policy", domain.McpCheckFail, "the host is on mcp.host_deny_list"))
		} else {
			out = append(out, check("host_policy", domain.McpCheckPass, ""))
		}
	}
	if x.Hosted != nil {
		out = append(out, check("image_digest", domain.McpCheckPass, "pinned by digest"))
		if domain.RegistryAllowed(x.Hosted.Image, st.AllowedRegistries) {
			out = append(out, check("image_signature", domain.McpCheckWarn, "verified by the reconciler before the pod starts"))
		}
	}
	if len(prims) == 0 {
		out = append(out, check("inventory", domain.McpCheckWarn, "the gateway lists the inventory within 2 minutes"))
	} else {
		out = append(out, check("inventory", domain.McpCheckPass, ""))
		ps := make([]mcp.Primitive, 0, len(prims))
		for _, p := range prims {
			ps = append(ps, toPrimitive(x.Slug, p))
		}
		if f := mcp.CheckDefinitions(ps, nil); len(f) > 0 {
			out = append(out, check("description_injection", domain.McpCheckFail, f[0].Check+": "+f[0].Primitive))
		} else {
			out = append(out, check("description_injection", domain.McpCheckPass, ""))
		}
	}
	return out
}

// Submit implements SubmitMcpServerForReview: runs the automatic checks and moves a draft (or a rejected server,
// through draft, as the database requires) to pending_review with its risk tier and approver.
func (s *Service) Submit(ctx context.Context, c acting.Caller, teamID, serverID string, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	const action = "SubmitMcpServerForReview"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return ServerView{}, nil, err
	}
	t := transition{action: action, scope: s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersWrite),
		change: func(st Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if cur.ReviewStatus != domain.McpDraft {
				return "", nil, domain.Conflict(domain.ReasonInvalidReviewTransition, "the server is "+string(cur.ReviewStatus),
					map[string]any{"reviewStatus": string(cur.ReviewStatus)})
			}
			next.ReviewStatus = domain.McpPendingReview
			return domain.EventMcpServerSubmitted, nil, nil
		},
		after: func(ctx context.Context, tx port.Tx, st Settings, stored domain.McpServerRecord) (domain.McpServerRecord, error) {
			prims, err := s.Repo.ListPrimitives(ctx, tx, stored.ID, PrimitiveFilter{}, "", domain.MaxMcpPrimitivesPerServer)
			if err != nil {
				return stored, err
			}
			next := stored
			next.Checks = runChecks(st, stored, prims, s.Now())
			assess(st, &next, prims)
			if stored, err = s.store(ctx, tx, next); err != nil {
				return stored, err
			}
			return stored, s.Repo.RequestRefresh(ctx, tx, stored.ID)
		}}
	var out ServerView
	var replay *port.StoredResponse
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || r != nil {
			replay = r
			return err
		}
		cur, err := s.Repo.GetServer(ctx, tx, serverID, true)
		if err != nil {
			return err
		}
		if cur.TeamID == teamID && cur.ReviewStatus == domain.McpRejected {
			next := cur
			next.ReviewStatus = domain.McpDraft
			if _, err := s.store(ctx, tx, next); err != nil {
				return err
			}
		}
		if out, err = s.runIn(ctx, tx, c, serverID, nil, t); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, replay, err
}

// TeamReview implements ApproveTeamMcpServer and RejectTeamMcpServer. A team-admin may decide only when
// mcp.team_server_approval leaves this tier to the team (403 approval_not_permitted_by_policy).
func (s *Service) TeamReview(ctx context.Context, c acting.Caller, teamID, serverID string, approve bool, in ReviewInput, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	to := domain.McpRejected
	if approve {
		to = domain.McpApproved
	}
	action, _ := reviewAction(to, "Team")
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersApprove); err != nil {
		return ServerView{}, nil, err
	}
	approver := "team_admin"
	if c.Principal.ServiceAdmin {
		approver = "security"
	}
	return s.review(ctx, c, action, serverID, in, to, false, approver, s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersApprove),
		func(st Settings, srv domain.McpServerRecord) error {
			if c.Principal.ServiceAdmin || domain.TeamAdminMayApprove(st.Approval, srv.RiskTier) {
				return nil
			}
			return s.refuse(ctx, c, action, teamID, authz.TeamMcpServersApprove, domain.ReasonApprovalNotPermittedByPolicy,
				"mcp.team_server_approval sends a "+srv.RiskTier+" risk server to security")
		}, idem)
}

// RefreshTeam implements RefreshTeamMcpInventory (team servers; → 202).
func (s *Service) RefreshTeam(ctx context.Context, c acting.Caller, teamID, serverID string) error {
	const action = "RefreshTeamMcpInventory"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return err
	}
	return s.refresh(ctx, c, action, serverID, s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersWrite))
}

// teamAccess is what a team's view of a server's primitives is decided with.
func (s *Service) teamAccess(ctx context.Context, tx port.Tx, teamID string, r domain.TeamMcpServerRow) (mcp.Access, error) {
	srv := r.Server
	a := mcp.Access{EnabledForAgent: true, TrustAnnotations: srv.TrustAnnotations,
		Published:      (srv.IsTeam() || srv.Published) && (srv.ReviewStatus == domain.McpApproved || srv.ReviewStatus == domain.McpSuspended),
		EnabledForTeam: r.Switch != nil && r.Switch.Enabled}
	svc, err := s.Repo.ListRules(ctx, tx, PolicyScope{ServerID: srv.ID, Level: "service"})
	if err != nil {
		return a, err
	}
	team, err := s.Repo.ListRules(ctx, tx, PolicyScope{ServerID: srv.ID, Level: "team", TeamID: teamID})
	if err != nil {
		return a, err
	}
	for _, x := range svc {
		a.Service.Rules = append(a.Service.Rules, x.Rule())
	}
	for _, x := range team {
		a.Team.Rules = append(a.Team.Rules, x.Rule())
	}
	a.Team.ReadOnly = r.Switch != nil && r.Switch.Mode == domain.McpTeamModeReadOnly
	return a, nil
}

// decide is the team's decision on one primitive, with the level that decided it.
func decide(a mcp.Access, srv domain.McpServerRecord, p domain.McpPrimitiveRecord) *domain.McpDecisionView {
	if srv.ReviewStatus == domain.McpSuspended {
		return &domain.McpDecisionView{Level: "service", Reason: domain.ReasonMcpServerSuspended}
	}
	d := a.Evaluate(toPrimitive(srv.Slug, p))
	if d.Allowed {
		return &domain.McpDecisionView{Allowed: true}
	}
	level := "team"
	switch d.Reason {
	case mcp.ReasonServerNotPublished, mcp.ReasonPendingReview:
		if !srv.IsTeam() {
			level = "service"
		}
	case mcp.ReasonBlockedService:
		level = "service"
	}
	return &domain.McpDecisionView{Level: level, Reason: string(d.Reason)}
}

// ListTeamPrimitives implements ListTeamMcpPrimitives: with the team's and service's decision per primitive.
func (s *Service) ListTeamPrimitives(ctx context.Context, c acting.Caller, teamID, serverID string, f PrimitiveFilter, p acting.Page) ([]PrimitiveView, string, error) {
	if err := s.checkTeam(ctx, c, "ListTeamMcpPrimitives", teamID, authz.TeamMcpServersRead); err != nil {
		return nil, "", err
	}
	if err := validPrimitiveFilter(f); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var out []PrimitiveView
	var next string
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		a, err := s.teamAccess(ctx, tx, teamID, r)
		if err != nil {
			return err
		}
		rows, err := s.Repo.ListPrimitives(ctx, tx, serverID, f, p.Cursor, p.Limit+1)
		if err != nil {
			return err
		}
		var page []domain.McpPrimitiveRecord
		page, next = acting.Slice(rows, p.Limit, func(x domain.McpPrimitiveRecord) string { return x.ID })
		out = make([]PrimitiveView, 0, len(page))
		for _, x := range page {
			out = append(out, PrimitiveView{Primitive: x, Decision: decide(a, r.Server, x)})
		}
		return nil
	})
	return out, next, err
}

// TeamDecidePrimitive implements ApproveTeamMcpPrimitive and RejectTeamMcpPrimitive on a team server. A team-admin
// may not approve a change that crosses a risk line, nor one whose tier the policy sends to security.
func (s *Service) TeamDecidePrimitive(ctx context.Context, c acting.Caller, teamID, serverID, primitiveID, note string, approve bool, idem *acting.Idem) (PrimitiveView, *port.StoredResponse, error) {
	action := "RejectTeamMcpPrimitive"
	if approve {
		action = "ApproveTeamMcpPrimitive"
	}
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersApprove); err != nil {
		return PrimitiveView{}, nil, err
	}
	approver := "team_admin"
	if c.Principal.ServiceAdmin {
		approver = "security"
	}
	return s.primitiveDecision(ctx, c, action, serverID, primitiveID, note, approve, approver,
		s.teamServerOf(ctx, c, action, teamID, authz.TeamMcpServersApprove),
		func(st Settings, srv domain.McpServerRecord, p domain.McpPrimitiveRecord) error {
			if c.Principal.ServiceAdmin || !approve {
				return nil
			}
			if buildChange(srv.Slug, p).CrossesRiskLine || !domain.TeamAdminMayApprove(st.Approval, srv.RiskTier) {
				return s.refuse(ctx, c, action, teamID, authz.TeamMcpServersApprove, domain.ReasonApprovalNotPermittedByPolicy,
					"this change needs a security review")
			}
			return nil
		}, idem)
}

// PutTeamPolicy implements PutTeamMcpPolicy: the team's rules for a server it can see.
func (s *Service) PutTeamPolicy(ctx context.Context, c acting.Caller, teamID, serverID string, rules []domain.McpPolicyRule, expect *domain.Version) (PolicyView, error) {
	if err := s.checkTeam(ctx, c, "PutTeamMcpPolicy", teamID, authz.TeamMcpServersWrite); err != nil {
		return PolicyView{}, err
	}
	if err := domain.ValidateMcpPolicyRules(rules, "rules"); err != nil {
		return PolicyView{}, err
	}
	var out PolicyView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		ver := r.Server.Version
		if !r.Server.IsTeam() && r.Switch != nil {
			ver = r.Switch.Version
		}
		if expect != nil {
			if err := domain.CheckVersion(ver, *expect); err != nil {
				return err
			}
		}
		if err := s.replaceRules(ctx, tx, c, "PutTeamMcpPolicy", r.Server, PolicyScope{ServerID: serverID, Level: "team", TeamID: teamID}, rules); err != nil {
			return err
		}
		out = PolicyView{ServerID: serverID, Level: "team", TeamID: teamID, Rules: rules, DriftPolicy: r.Server.DriftPolicy,
			AllowSampling: r.Server.AllowSampling, TrustAnnotations: r.Server.TrustAnnotations, Version: ver}
		return nil
	})
	return out, err
}

// PutTeamCredential implements PutTeamMcpServerCredential: a team server's credential, or the team's credential of
// a per_team enterprise server.
func (s *Service) PutTeamCredential(ctx context.Context, c acting.Caller, teamID, serverID string, in CredentialInput, expect *domain.Version) (CredentialView, error) {
	const action = "PutTeamMcpServerCredential"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return CredentialView{}, err
	}
	var visible bool
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		_, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		visible = err == nil
		return err
	})
	if !visible {
		return CredentialView{}, err
	}
	return s.putCredential(ctx, c, action, serverID, teamID, in, expect, func(srv domain.McpServerRecord) (string, error) {
		if !srv.IsTeam() && srv.CredentialMode == domain.McpCredentialShared {
			return "", domain.Validation(domain.FieldError{Field: "credential", Reason: "the service manages this server's shared credential"})
		}
		return teamID, nil
	})
}

// DisconnectOAuth implements DisconnectMcpOAuth: the team's user-delegated credential is removed and its secret
// scheduled for deletion.
func (s *Service) DisconnectOAuth(ctx context.Context, c acting.Caller, teamID, serverID string, idem *acting.Idem) (CredentialView, *port.StoredResponse, error) {
	const action = "DisconnectMcpOAuth"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return CredentialView{}, nil, err
	}
	var out CredentialView
	var replay *port.StoredResponse
	var arn string
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || r != nil {
			replay = r
			return err
		}
		row, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		if row.Server.AuthKind != domain.McpAuthOAuthUser {
			return domain.Validation(domain.FieldError{Field: "authKind", Reason: domain.ReasonAuthKindUnsupported})
		}
		scope := row.Server.CredentialScope(teamID)
		cur, err := s.Repo.GetCredential(ctx, tx, serverID, scope)
		if err != nil {
			return err
		}
		if cur != nil {
			if err := s.Repo.DeleteCredential(ctx, tx, serverID, scope); err != nil {
				return err
			}
			arn = cur.SecretARN
			if err := s.Audit.Write(ctx, tx, c.Record(action, kindCredential, cur.ID, teamID,
				map[string]any{"scope": scope, "kind": cur.Kind, "state": cur.Status, "subject": cur.Subject}, nil)); err != nil {
				return err
			}
			if err := s.Emit(ctx, tx, c, domain.McpCredentialEvent(false), teamID, kindCredential, cur.ID, map[string]any{
				"serverId": serverID, "scope": scope, "kind": cur.Kind, "state": domain.McpCredMissing}); err != nil {
				return err
			}
		}
		out = credentialView(scope, row.Server.AuthKind, nil)
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	if err == nil && arn != "" {
		_ = s.Secrets.ScheduleDeletion(context.WithoutCancel(ctx), arn, 7)
	}
	return out, replay, err
}

// TestFederation implements TestMcpFederation (→ 202): the gateway tries the exchange and records the state
// (§4.12: audited, no event).
func (s *Service) TestFederation(ctx context.Context, c acting.Caller, teamID, serverID string) (TeamServerView, error) {
	const action = "TestMcpFederation"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return TeamServerView{}, err
	}
	var out TeamServerView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		row, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		if err != nil {
			return err
		}
		if row.Server.AuthKind != domain.McpAuthWorkloadFederation {
			return domain.Validation(domain.FieldError{Field: "authKind", Reason: domain.ReasonAuthKindUnsupported})
		}
		if err := s.Repo.RequestFederationTest(ctx, tx, serverID, teamID); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindFederation, serverID+"/"+teamID, teamID, nil,
			map[string]any{"serverId": serverID, "testRequested": true})); err != nil {
			return err
		}
		if row, err = s.Repo.GetTeamServer(ctx, tx, teamID, serverID); err != nil {
			return err
		}
		out = teamView(st, row)
		return nil
	})
	return out, err
}

// EffectiveServer is one server of an agent's or pool's effective MCP access.
type EffectiveServer struct {
	ServerID   string
	Slug       string
	Scope      string
	Mode       string
	URL        string
	Primitives []domain.McpAdmissionRow
}

// Effective is GetEffectiveAgentMcp and GetEffectivePoolMcp.
type Effective struct {
	Subject domain.McpSubject
	Servers []EffectiveServer
	Env     []string
}

// GetEffective implements GetEffectiveAgentMcp (kind agent) and GetEffectivePoolMcp (kind pool), from
// afs.mcp_admission (CD-51): every primitive with the deciding level and reason.
func (s *Service) GetEffective(ctx context.Context, c acting.Caller, teamID, kind, id string) (Effective, error) {
	action := "GetEffectiveAgentMcp"
	if kind == "pool" {
		action = "GetEffectivePoolMcp"
	}
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersRead); err != nil {
		return Effective{}, err
	}
	var out Effective
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		subj, err := s.Repo.Subject(ctx, tx, teamID, kind, id)
		if err != nil {
			return err
		}
		if subj.Mode == "" {
			subj.Mode = st.DefaultAgentMode
		}
		rows, err := s.Repo.Admission(ctx, tx, kind, id)
		if err != nil {
			return err
		}
		out = Effective{Subject: subj, Servers: []EffectiveServer{}}
		var slugs []string
		for _, r := range rows {
			n := len(out.Servers)
			if n == 0 || out.Servers[n-1].ServerID != r.ServerID {
				out.Servers = append(out.Servers, EffectiveServer{ServerID: r.ServerID, Slug: r.Slug, Scope: r.ServerKind, Mode: r.Mode,
					URL: "/mcp/" + r.Slug})
				n++
			}
			out.Servers[n-1].Primitives = append(out.Servers[n-1].Primitives, r)
			if r.Allowed {
				slugs = append(slugs, r.Slug)
			}
		}
		out.Env = domain.McpEnv(slugs)
		return nil
	})
	return out, err
}
