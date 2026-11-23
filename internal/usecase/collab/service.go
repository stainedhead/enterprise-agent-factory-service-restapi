// Package collab implements the orchestration and collaboration operations of API contract §4.4b and
// §4.4d (RA-17, RA-US-20): GetTeamOrchestration, PutTeamOrchestration, ChangeCollaborationTool,
// ListOrchestrationEndpoints, the deprecated GetTeamCollaboration, PutTeamCollaboration and
// ListCollaborationEndpoints, ListCollaborationLinks, ConnectAgentToCollaboration,
// DisconnectAgentFromCollaboration and RotateCollaborationKeys.
//
// Every team has one orchestration agent that runs the collaboration tool; these operations configure it. The API only writes
// desired state: the reconciler renders Paperclip, the Service and the network policies and observes
// the link state. A secret value is generated here only for RotateCollaborationKeys, written to Secrets
// Manager and dropped: it never reaches a response, audit record, event or log line.
package collab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindCollab = "team_collaboration"
	kindLink   = "agent_collaboration"
)

// LinkFilter narrows ListCollaborationLinks.
type LinkFilter struct {
	State   domain.LinkState
	Connect *bool
}

// EndpointFilter narrows ListOrchestrationEndpoints (and the deprecated ListCollaborationEndpoints).
type EndpointFilter struct {
	TeamID string
	State  domain.Phase
	// Exposure keeps surfaces whose effective level is this (private or public); empty keeps all.
	Exposure domain.Exposure
	// Tool keeps one tool's teams; empty keeps all.
	Tool domain.CollabTool
}

// AgentRef is what the connect and disconnect checks need to know about an agent.
type AgentRef struct {
	ID    string
	Name  string
	Role  domain.AgentRole
	State domain.AgentState
	Pool  bool
}

// ToolRuntime is the orchestration.tools entry of a tool and the catalog template it names.
type ToolRuntime struct {
	Tool            domain.CollabTool
	Enabled         bool
	TemplateID      string
	ImageRepository string
	// Harness and Digest come from the template: the agent's template column and current image digest.
	Harness string
	Digest  string
	// TemplateEnabled is the template's enabled flag.
	TemplateEnabled bool
}

// Ready reports whether the tool's runtime is set up (orchestration_runtime_unset otherwise).
func (r ToolRuntime) Ready() bool {
	return r.TemplateID != "" && r.ImageRepository != "" && r.TemplateEnabled && r.Digest != ""
}

// Repository is the collaboration store. Every method runs in the transaction it is given, so
// row-level security applies with the caller's session.
type Repository interface {
	// Get returns the team's orchestration resource with the team, agent and provider facts the
	// computation needs; ok is false when the team has no orchestration agent. forUpdate locks the row.
	Get(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (c domain.TeamCollaboration, ok bool, err error)
	// Update stores the exposure choices and the registration mode; the database guard rejects a level
	// above the limit or one the tool lacks.
	Update(ctx context.Context, tx port.Tx, teamID string, webUI, webAPI domain.Exposure, mode domain.RegistrationMode) error
	// ListLinks returns one link per live worker agent of the team with id greater than after.
	ListLinks(ctx context.Context, tx port.Tx, teamID string, f LinkFilter, after string, limit int) ([]domain.CollaborationLink, error)
	// Agent returns a live agent of the team; ok is false when there is none.
	Agent(ctx context.Context, tx port.Tx, teamID, agentID string) (AgentRef, bool, error)
	// Link returns the worker's link (a never-connected worker is connect false, disabled).
	Link(ctx context.Context, tx port.Tx, teamID, agentID string, forUpdate bool) (domain.CollaborationLink, error)
	// SetLink inserts or updates the worker's connection (the database guard rejects a bad row).
	SetLink(ctx context.Context, tx port.Tx, teamID, agentID string, connect bool, paperclipAgentID *string) error
	// BumpAgent raises the orchestration agent's generation so its pod rolls.
	BumpAgent(ctx context.Context, tx port.Tx, teamID, agentID string) error
	// BootSecret returns the AFS-managed secret of that name on the orchestration agent.
	BootSecret(ctx context.Context, tx port.Tx, teamID, agentID, name string) (SecretRef, bool, error)
	// SetSecretVersion records the new Secrets Manager version of a secret.
	SetSecretVersion(ctx context.Context, tx port.Tx, teamID, secretID, versionID string, at time.Time) error
	// Endpoints returns the service-wide endpoint rows (a team with a stored surface not off) with team
	// id greater than after.
	Endpoints(ctx context.Context, tx port.Tx, f EndpointFilter, after string, limit int) ([]EndpointRow, error)
	// Migration counts the teams with and without an orchestration agent and lists the blocked ones.
	Migration(ctx context.Context, tx port.Tx) (total, with int, blocked []domain.OrchestrationBlocked, err error)
	// ToolRuntime reads the tool's orchestration.tools entry and its template.
	ToolRuntime(ctx context.Context, tx port.Tx, tool domain.CollabTool) (ToolRuntime, error)
	// SwitchTool inserts the pre_tool_change snapshot request, sets the tool, the agent's template and
	// image, the surfaces the new tool lacks to off and the registration mode, in the caller's
	// transaction. It returns the snapshot id.
	SwitchTool(ctx context.Context, tx port.Tx, in SwitchToolInput) (snapshotID string, err error)
}

// SwitchToolInput is what SwitchTool writes.
type SwitchToolInput struct {
	TeamID, AgentID, SnapshotID string
	FromTool, ToTool            domain.CollabTool
	Runtime                     ToolRuntime
	Mode                        domain.RegistrationMode
	RetainUntil                 time.Time
	RequestedBy                 string
}

// SecretRef is an AFS-managed secret row.
type SecretRef struct {
	ID  string
	ARN string
}

// EndpointRow is the stored side of an endpoint: the team's orchestration resource, its phase and the
// last time someone used it. The service computes the rest.
type EndpointRow struct {
	Collab       domain.TeamCollaboration
	Phase        domain.Phase
	LastAccessAt *time.Time
}

// BootSecretWriter writes the boot secrets of a tool to Secrets Manager and records them on the
// orchestration agent, in the caller's transaction (implemented by the agents use case, which also
// creates them for a new team). Compensate deletes what a failed request wrote.
type BootSecretWriter interface {
	CreateBootSecrets(ctx context.Context, tx port.Tx, c acting.Caller, teamID, agentID string, tool domain.CollabTool, idem *acting.Idem, created *[]string) error
	Compensate(ctx context.Context, arns []string)
}

// ValueWriter writes a new Secrets Manager version (the only AWS call this package makes).
type ValueWriter interface {
	PutValue(ctx context.Context, arn string, value domain.SecretValue, token string) (string, error)
}

// Service is the collaboration use case set.
type Service struct {
	acting.Deps
	Repo     Repository
	Settings port.SettingsReader
	Writer   ValueWriter
	// Boot writes the new tool's boot secrets on a tool change; nil refuses a change to a tool that has
	// boot secrets.
	Boot BootSecretWriter
	// Rand is the random source for generated keys (crypto/rand.Reader in production).
	Rand io.Reader
}

// View is the resource with the computed fields.
type View struct {
	Collab   domain.TeamCollaboration
	Settings domain.OrchestrationSettings
	WebUI    domain.SurfaceState
	WebAPI   domain.SurfaceState
}

// LinkResult is a link with the replayed response of an idempotent call.
type LinkResult struct {
	Link   domain.CollaborationLink
	Replay *port.StoredResponse
}

// RotatedSecret is one rotated boot secret.
type RotatedSecret struct {
	SecretID  string
	Name      string
	VersionID string
}

// RotateResult is the result of RotateCollaborationKeys.
type RotateResult struct {
	Rotated []RotatedSecret
	Replay  *port.StoredResponse
}

// Endpoint is one surface of a team in ListOrchestrationEndpoints.
type Endpoint struct {
	Row     EndpointRow
	Surface domain.Surface
	State   domain.SurfaceState
}

// EndpointsResult is a page of endpoints with the backfill report (first page only).
type EndpointsResult struct {
	Items     []Endpoint
	Next      string
	Migration domain.OrchestrationMigration
}

// ChangeToolResult is the result of ChangeCollaborationTool.
type ChangeToolResult struct {
	View       View
	SnapshotID string
	Replay     *port.StoredResponse
}

func (s *Service) settings(ctx context.Context) (domain.OrchestrationSettings, error) {
	return LoadSettings(ctx, s.Settings)
}

// LoadSettings reads the four orchestration.* settings the effective levels depend on.
func LoadSettings(ctx context.Context, rd port.SettingsReader) (domain.OrchestrationSettings, error) {
	var out domain.OrchestrationSettings
	str := func(key string) (string, error) {
		raw, err := rd.Setting(ctx, key)
		if err != nil {
			return "", err
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", domain.Internal(err)
		}
		if v == nil {
			return "", nil
		}
		return *v, nil
	}
	flag := func(key string) (bool, error) {
		raw, err := rd.Setting(ctx, key)
		if err != nil {
			return false, err
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, domain.Internal(err)
		}
		return v, nil
	}
	var err error
	if out.PublicBaseDomain, err = str(domain.SettingOrchestrationPublicBaseDomain); err != nil {
		return out, err
	}
	if out.PrivateBaseDomain, err = str(domain.SettingOrchestrationPrivateBaseDomain); err != nil {
		return out, err
	}
	if out.PublicEndpoints, err = flag(domain.SettingOrchestrationPublicEndpoints); err != nil {
		return out, err
	}
	out.PrivateEndpoints, err = flag(domain.SettingOrchestrationPrivateEndpoints)
	return out, err
}

func (s *Service) view(ctx context.Context, c domain.TeamCollaboration) (View, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return View{}, err
	}
	ui, api := c.Surfaces(set)
	return View{Collab: c, Settings: set, WebUI: ui, WebAPI: api}, nil
}

// manage is the use case's own check of collaboration:manage (the route's middleware decides first).
func (s *Service) check(ctx context.Context, c acting.Caller, perm authz.Permission, action, teamID string) error {
	switch d := authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}); d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

func auditOf(c domain.TeamCollaboration) map[string]any {
	return map[string]any{"agentId": c.AgentID, "tool": string(c.Tool), "webUiExposure": string(c.WebUIExposure),
		"webApiExposure": string(c.WebAPIExposure), "registrationMode": string(c.Mode), "limit": string(c.Limit)}
}

// Get implements GetTeamOrchestration and the deprecated GetTeamCollaboration.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string) (View, error) {
	if err := s.check(ctx, c, authz.CollaborationRead, "GetTeamOrchestration", teamID); err != nil {
		return View{}, err
	}
	var col domain.TeamCollaboration
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, ok, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if !ok {
			return domain.OrchestrationPending(false)
		}
		col = cur
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, col)
}

// Put implements PutTeamOrchestration. expect is nil when If-Match is absent (it is optional here).
// A level above the team's limit or on a surface the tool lacks is refused; lowering a level is always
// accepted. An unset base domain, a kill switch or a provider without a collaboration client does not
// refuse the write: the choice is stored and the effective level says why it is not served (CD-27).
// One transaction writes the row, the audit record and orchestration.updated.
func (s *Service) Put(ctx context.Context, c acting.Caller, teamID string, p domain.PutOrchestrationParams, expect *domain.Version) (View, error) {
	return s.put(ctx, c, "PutTeamOrchestration", teamID, p, expect, nil)
}

// LegacyPut implements the deprecated PutTeamCollaboration: the booleans become levels (true is the
// highest level within the limit, false is off), then the write is PutTeamOrchestration's. It also emits
// the deprecated collaboration.updated.
func (s *Service) LegacyPut(ctx context.Context, c acting.Caller, teamID string, webUI, restAPI bool, mode domain.RegistrationMode, expect *domain.Version) (View, error) {
	return s.put(ctx, c, "PutTeamCollaboration", teamID, domain.PutOrchestrationParams{}, expect, &legacyPut{webUI: webUI, restAPI: restAPI, mode: mode})
}

type legacyPut struct {
	webUI, restAPI bool
	mode           domain.RegistrationMode
}

func (s *Service) put(ctx context.Context, c acting.Caller, action, teamID string, p domain.PutOrchestrationParams, expect *domain.Version, legacy *legacyPut) (View, error) {
	if err := s.check(ctx, c, authz.CollaborationManage, action, teamID); err != nil {
		return View{}, err
	}
	var out domain.TeamCollaboration
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, ok, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.OrchestrationPending(true)
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if legacy != nil {
			if p, err = cur.LegacyPutToOrchestration(legacy.webUI, legacy.restAPI, legacy.mode); err != nil {
				return err
			}
		}
		ready := cur.Status.Phase == domain.PhaseReady
		if err := cur.ValidatePutOrchestration(p, ready); err != nil {
			return err
		}
		mode := cur.Mode
		if p.Mode != "" {
			mode = p.Mode
		}
		changes := map[string]any{}
		change := func(name string, from, to string) {
			if from != to {
				changes[name] = map[string]any{"from": from, "to": to}
			}
		}
		change("webUiExposure", string(cur.WebUIExposure), string(p.WebUI))
		change("webApiExposure", string(cur.WebAPIExposure), string(p.WebAPI))
		change("registrationMode", string(cur.Mode), string(mode))
		if err := s.Repo.Update(ctx, tx, teamID, p.WebUI, p.WebAPI, mode); err != nil {
			return err
		}
		next, _, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindCollab, teamID, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		data := map[string]any{"teamId": teamID, "agentId": cur.AgentID, "changes": changes}
		if err := s.Emit(ctx, tx, c, domain.EventOrchestrationUpdated, teamID, kindCollab, teamID, data); err != nil {
			return err
		}
		if legacy != nil {
			old := map[string]any{}
			if _, ok := changes["webUiExposure"]; ok {
				old["webUiEnabled"] = map[string]any{"from": cur.WebUIExposure != domain.ExposureOff, "to": p.WebUI != domain.ExposureOff}
			}
			if _, ok := changes["webApiExposure"]; ok {
				old["restApiEnabled"] = map[string]any{"from": cur.WebAPIExposure != domain.ExposureOff, "to": p.WebAPI != domain.ExposureOff}
			}
			if v, ok := changes["registrationMode"]; ok {
				old["registrationMode"] = v
			}
			if err := s.Emit(ctx, tx, c, domain.EventCollaborationUpdated, teamID, kindCollab, teamID,
				map[string]any{"teamId": teamID, "agentId": cur.AgentID, "changes": old}); err != nil {
				return err
			}
		}
		out = next
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, out)
}

// ChangeTool implements ChangeCollaborationTool (ORC-11). In one transaction it inserts the
// pre_tool_change snapshot request (kept 90 days, labelled with the old tool), sets the tool, the
// agent's template and image from orchestration.tools, sets every surface the new tool lacks to off and,
// for a tool with boot secrets, writes them (generated in Secrets Manager first; failure is a 503 and
// nothing changes). The reconciler then takes the snapshot, starts the new tool empty and re-registers
// the connected workers.
func (s *Service) ChangeTool(ctx context.Context, c acting.Caller, teamID string, tool domain.CollabTool, idem *acting.Idem) (ChangeToolResult, error) {
	if err := s.check(ctx, c, authz.CollaborationManage, "ChangeCollaborationTool", teamID); err != nil {
		return ChangeToolResult{}, err
	}
	if !tool.Valid() {
		return ChangeToolResult{}, domain.Validation(domain.FieldError{Field: "tool", Reason: "must be paperclip or buzz"})
	}
	var res ChangeToolResult
	var created []string
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = ChangeToolResult{}
		created = nil
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, ok, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.OrchestrationPending(true)
		}
		switch {
		case cur.Tool == tool:
			return domain.Validation(domain.FieldError{Field: "tool", Reason: domain.ReasonToolUnchanged})
		case toolChanging(cur.Status):
			return domain.Conflict(domain.ReasonToolChangeInProgress, "a tool change is already running", nil)
		}
		rt, err := s.Repo.ToolRuntime(ctx, tx, tool)
		if err != nil {
			return err
		}
		switch {
		case !rt.Enabled:
			return domain.Validation(domain.FieldError{Field: "tool", Reason: domain.ReasonToolNotEnabled})
		case !rt.Ready():
			return domain.Conflict(domain.ReasonOrchestrationRuntimeUnset, "the runtime of tool "+string(tool)+" is not set up", nil)
		}
		sid, err := s.IDs.NewID(domain.PrefixSnapshot)
		if err != nil {
			return err
		}
		mode := cur.Mode
		if !tool.SupportsRegistration(mode) {
			mode = tool.DefaultRegistration()
		}
		if _, err := s.Repo.SwitchTool(ctx, tx, SwitchToolInput{TeamID: teamID, AgentID: cur.AgentID, SnapshotID: sid.String(),
			FromTool: cur.Tool, ToTool: tool, Runtime: rt, Mode: mode, RetainUntil: s.Now().Add(90 * 24 * time.Hour), RequestedBy: c.Session().Actor}); err != nil {
			return err
		}
		if boots := tool.BootSecrets(); len(boots) > 0 {
			if s.Boot == nil || s.Rand == nil {
				return domain.Unavailable("Secrets Manager", errNoBoot)
			}
			if err := s.Boot.CreateBootSecrets(ctx, tx, c, teamID, cur.AgentID, tool, idem, &created); err != nil {
				return err
			}
		}
		next, _, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("ChangeCollaborationTool", kindCollab, teamID, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventOrchestrationToolChanged, teamID, kindCollab, teamID,
			map[string]any{"teamId": teamID, "agentId": cur.AgentID, "fromTool": string(cur.Tool), "toTool": string(tool), "snapshotId": sid.String()}); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventAgentUpdated, teamID, "agents", cur.AgentID,
			map[string]any{"agentId": cur.AgentID, "changed": []string{"template", "image"}}); err != nil {
			return err
		}
		res.SnapshotID = sid.String()
		res.View, err = s.viewIn(ctx, next)
		if err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.View)
	})
	if err != nil && s.Boot != nil {
		s.Boot.Compensate(ctx, created)
	}
	return res, err
}

var errNoBoot = errors.New("collab: no Secrets Manager writer for the boot secrets")

// toolChanging reports whether the ToolChanging condition is True (a change is running).
func toolChanging(st domain.Status) bool {
	for _, c := range st.Conditions {
		if c.Type == "ToolChanging" && c.Status == domain.ConditionTrue {
			return true
		}
	}
	return false
}

func (s *Service) viewIn(ctx context.Context, c domain.TeamCollaboration) (View, error) {
	return s.view(ctx, c)
}

// Links implements ListCollaborationLinks.
func (s *Service) Links(ctx context.Context, c acting.Caller, teamID string, f LinkFilter, p acting.Page) ([]domain.CollaborationLink, string, error) {
	if err := s.check(ctx, c, authz.CollaborationRead, "ListCollaborationLinks", teamID); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.CollaborationLink
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, ok, err := s.Repo.Get(ctx, tx, teamID, false); err != nil {
			return err
		} else if !ok {
			return domain.OrchestrationPending(false)
		}
		var err error
		rows, err = s.Repo.ListLinks(ctx, tx, teamID, f, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(l domain.CollaborationLink) string { return l.AgentID })
	return page, next, nil
}

// Connect implements ConnectAgentToCollaboration. paperclipAgentID is accepted only in manual mode.
func (s *Service) Connect(ctx context.Context, c acting.Caller, teamID, agentID, paperclipAgentID string, idem *acting.Idem) (LinkResult, error) {
	return s.setConnect(ctx, c, "ConnectAgentToCollaboration", teamID, agentID, true, paperclipAgentID, idem)
}

// Disconnect implements DisconnectAgentFromCollaboration.
func (s *Service) Disconnect(ctx context.Context, c acting.Caller, teamID, agentID string, idem *acting.Idem) (LinkResult, error) {
	return s.setConnect(ctx, c, "DisconnectAgentFromCollaboration", teamID, agentID, false, "", idem)
}

func (s *Service) setConnect(ctx context.Context, c acting.Caller, action, teamID, agentID string, connect bool, pcID string, idem *acting.Idem) (LinkResult, error) {
	if err := s.check(ctx, c, authz.CollaborationManage, action, teamID); err != nil {
		return LinkResult{}, err
	}
	if err := domain.ValidatePaperclipAgentID(pcID); err != nil {
		return LinkResult{}, err
	}
	var res LinkResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = LinkResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		col, ok, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.OrchestrationPending(true)
		}
		ag, ok, err := s.Repo.Agent(ctx, tx, teamID, agentID)
		if err != nil {
			return err
		}
		switch {
		case !ok:
			return domain.NotFound("agent")
		case ag.Pool:
			return domain.Validation(domain.FieldError{Field: "agentId", Reason: "a pool cannot connect to Paperclip"})
		case ag.Role.Normalize() == domain.AgentRoleOrchestration:
			return domain.Validation(domain.FieldError{Field: "agentId", Reason: "the orchestration agent cannot be connected to itself"})
		}
		if pcID != "" && col.Mode == domain.RegistrationManaged {
			return domain.Validation(domain.FieldError{Field: "paperclipAgentId", Reason: domain.ReasonManagedRegistration})
		}
		before, err := s.Repo.Link(ctx, tx, teamID, agentID, true)
		if err != nil {
			return err
		}
		var set *string
		if connect && pcID != "" {
			set = &pcID
		}
		if err := s.Repo.SetLink(ctx, tx, teamID, agentID, connect, set); err != nil {
			return err
		}
		after, err := s.Repo.Link(ctx, tx, teamID, agentID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindLink, agentID, teamID, linkAudit(before), linkAudit(after))); err != nil {
			return err
		}
		res.Link = after
		return idem.Complete(ctx, s.Idem, tx, after)
	})
	return res, err
}

func linkAudit(l domain.CollaborationLink) map[string]any {
	return map[string]any{"connect": l.Connect, "paperclipAgentId": l.PaperclipAgentID, "linkState": string(l.LinkState)}
}

// Rotate implements RotateCollaborationKeys: a new random value (32 bytes, hex) for each named signing
// secret, written to Secrets Manager with PutSecretValue; the master key and any other name are refused.
func (s *Service) Rotate(ctx context.Context, c acting.Caller, teamID string, names []string, idem *acting.Idem) (RotateResult, error) {
	if err := s.check(ctx, c, authz.CollaborationManage, "RotateCollaborationKeys", teamID); err != nil {
		return RotateResult{}, err
	}
	if len(names) == 0 {
		names = domain.RotatableCollabKeys()
	}
	var fe []domain.FieldError
	for _, n := range names {
		switch {
		case n == domain.SecretPaperclipMasterKey:
			fe = append(fe, domain.FieldError{Field: "names", Reason: domain.ReasonMasterKeyNotRotatable})
		case !slices.Contains(domain.RotatableCollabKeys(), n):
			fe = append(fe, domain.FieldError{Field: "names", Reason: "unknown key " + n})
		}
	}
	if len(fe) > 0 {
		return RotateResult{}, domain.Validation(fe...)
	}
	var res RotateResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = RotateResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		col, ok, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.OrchestrationPending(true)
		}
		if col.Tool != domain.ToolPaperclip {
			return domain.Validation(domain.FieldError{Field: "names", Reason: "tool_has_no_rotatable_keys"})
		}
		for _, n := range names {
			sec, ok, err := s.Repo.BootSecret(ctx, tx, teamID, col.AgentID, n)
			if err != nil {
				return err
			}
			if !ok {
				return domain.NotFound("secret " + n)
			}
			val, err := s.newKey()
			if err != nil {
				return err
			}
			vid, err := s.Writer.PutValue(ctx, sec.ARN, val, rotateToken(c, idem, teamID, n))
			if err != nil {
				e := domain.Unavailable("Secrets Manager", err)
				e.RetryAfterSeconds = 1
				return e
			}
			if err := s.Repo.SetSecretVersion(ctx, tx, teamID, sec.ID, vid, s.Now()); err != nil {
				return err
			}
			if err := s.Emit(ctx, tx, c, domain.EventSecretRotated, teamID, "secrets", sec.ID,
				map[string]any{"secretId": sec.ID, "name": n, "versionId": vid}); err != nil {
				return err
			}
			res.Rotated = append(res.Rotated, RotatedSecret{SecretID: sec.ID, Name: n, VersionID: vid})
		}
		if err := s.Repo.BumpAgent(ctx, tx, teamID, col.AgentID); err != nil {
			return err
		}
		rotated := make([]string, 0, len(res.Rotated))
		for _, r := range res.Rotated {
			rotated = append(rotated, r.Name)
		}
		if err := s.Audit.Write(ctx, tx, c.Record("RotateCollaborationKeys", kindCollab, teamID, teamID, nil, map[string]any{"rotated": rotated})); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res)
	})
	return res, err
}

func (s *Service) newKey() (domain.SecretValue, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(s.Rand, b); err != nil {
		return domain.SecretValue{}, domain.Internal(err)
	}
	return domain.NewSecretValue("value", hex.EncodeToString(b))
}

func rotateToken(c acting.Caller, idem *acting.Idem, parts ...string) string {
	h := sha256.New()
	seed := "req:" + c.RequestID
	if idem != nil {
		seed = "idem:" + idem.Key.Key
	}
	for _, p := range append([]string{c.Principal.ID, seed, "RotateCollaborationKeys"}, parts...) {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	b := h.Sum(nil)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Endpoints implements ListOrchestrationEndpoints (service admins; the route's permission is
// admin:settings): one item per team and surface whose stored exposure is not off, with the effective
// level and reasons computed from the settings. The filters on the effective level and tool are applied
// here, so the page is filled from as many batches as it takes.
func (s *Service) Endpoints(ctx context.Context, c acting.Caller, f EndpointFilter, p acting.Page) (EndpointsResult, error) {
	if d := authz.Authorize(c.Principal, authz.AdminSettings, authz.Resource{Scope: authz.ScopeService}); !d.Allowed() {
		return EndpointsResult{}, s.Denied(ctx, c, "ListOrchestrationEndpoints", "", authz.AdminSettings)
	}
	set, err := s.settings(ctx)
	if err != nil {
		return EndpointsResult{}, err
	}
	p = p.Normalized()
	var out EndpointsResult
	more := false
	err = s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		out = EndpointsResult{}
		after := p.Cursor
		for {
			rows, err := s.Repo.Endpoints(ctx, tx, f, after, p.Limit+1)
			if err != nil {
				return err
			}
			for _, r := range rows {
				after = r.Collab.TeamID
				ui, api := r.Collab.Surfaces(set)
				for _, it := range []Endpoint{{Row: r, Surface: domain.SurfaceWebUI, State: ui}, {Row: r, Surface: domain.SurfaceWebAPI, State: api}} {
					if it.State.Exposure == domain.ExposureOff || (f.Exposure != "" && it.State.Effective != f.Exposure) {
						continue
					}
					out.Items = append(out.Items, it)
				}
			}
			more = len(rows) > p.Limit
			if !more || len(out.Items) > p.Limit {
				break
			}
		}
		if p.Cursor == "" {
			total, with, blocked, err := s.Repo.Migration(ctx, tx)
			if err != nil {
				return err
			}
			out.Migration = domain.ComputeMigrationState(total, with, blocked)
		}
		return nil
	})
	if err != nil {
		return EndpointsResult{}, err
	}
	if len(out.Items) > p.Limit {
		// Cut at a team boundary so a team's two surfaces are never split across pages (the cursor is a
		// team id).
		cut := p.Limit
		for cut < len(out.Items) && out.Items[cut].Row.Collab.TeamID == out.Items[cut-1].Row.Collab.TeamID {
			cut++
		}
		if cut < len(out.Items) || more {
			out.Next = out.Items[cut-1].Row.Collab.TeamID
		}
		out.Items = out.Items[:cut]
	}
	return out, nil
}
