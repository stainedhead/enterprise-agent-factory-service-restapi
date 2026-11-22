// Package collab implements the Paperclip collaboration operations of API contract §4.4b (RA-17):
// GetTeamCollaboration, PutTeamCollaboration, ListCollaborationLinks, ConnectAgentToCollaboration,
// DisconnectAgentFromCollaboration, RotateCollaborationKeys and ListCollaborationEndpoints.
//
// A team has at most one collaboration agent; these operations configure it. The API only writes
// desired state: the reconciler renders Paperclip, the Service and the network policies and observes
// the link state. A secret value is generated here only for RotateCollaborationKeys, written to Secrets
// Manager and dropped: it never reaches a response, audit record, event or log line.
package collab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// EndpointFilter narrows ListCollaborationEndpoints.
type EndpointFilter struct {
	TeamID string
	State  domain.Phase
}

// AgentRef is what the connect and disconnect checks need to know about an agent.
type AgentRef struct {
	ID    string
	Name  string
	Role  domain.AgentRole
	State domain.AgentState
	Pool  bool
}

// Repository is the collaboration store. Every method runs in the transaction it is given, so
// row-level security applies with the caller's session.
type Repository interface {
	// Get returns the team's collaboration resource with the team, agent and provider facts the
	// computation needs; ok is false when the team has no collaboration agent. forUpdate locks the row.
	Get(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (c domain.TeamCollaboration, ok bool, err error)
	// Update stores the stored choices; PreconditionFailed when the version is not expect (when set).
	Update(ctx context.Context, tx port.Tx, teamID string, web, rest bool, mode domain.RegistrationMode) error
	// ListLinks returns one link per live worker agent of the team with id greater than after.
	ListLinks(ctx context.Context, tx port.Tx, teamID string, f LinkFilter, after string, limit int) ([]domain.CollaborationLink, error)
	// Agent returns a live agent of the team; ok is false when there is none.
	Agent(ctx context.Context, tx port.Tx, teamID, agentID string) (AgentRef, bool, error)
	// Link returns the worker's link (a never-connected worker is connect false, disabled).
	Link(ctx context.Context, tx port.Tx, teamID, agentID string, forUpdate bool) (domain.CollaborationLink, error)
	// SetLink inserts or updates the worker's connection (the database guard rejects a bad row).
	SetLink(ctx context.Context, tx port.Tx, teamID, agentID string, connect bool, paperclipAgentID *string) error
	// BumpAgent raises the collaboration agent's generation so its pod rolls.
	BumpAgent(ctx context.Context, tx port.Tx, teamID, agentID string) error
	// BootSecret returns the AFS-managed secret of that name on the collaboration agent.
	BootSecret(ctx context.Context, tx port.Tx, teamID, agentID, name string) (SecretRef, bool, error)
	// SetSecretVersion records the new Secrets Manager version of a secret.
	SetSecretVersion(ctx context.Context, tx port.Tx, teamID, secretID, versionID string, at time.Time) error
	// Endpoints returns the service-wide endpoint rows with team id greater than after.
	Endpoints(ctx context.Context, tx port.Tx, f EndpointFilter, after string, limit int) ([]EndpointRow, error)
}

// SecretRef is an AFS-managed secret row.
type SecretRef struct {
	ID  string
	ARN string
}

// EndpointRow is the stored side of a CollaborationEndpoint; the service computes the rest.
type EndpointRow struct {
	Collab       domain.TeamCollaboration
	Phase        domain.Phase
	LastAccessAt *time.Time
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
	// Rand is the random source for generated keys (crypto/rand.Reader in production).
	Rand io.Reader
}

// View is the resource with the computed fields.
type View struct {
	Collab    domain.TeamCollaboration
	Settings  domain.CollabSettings
	Effective domain.Effective
	Hostname  string
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

// Endpoint is a row of ListCollaborationEndpoints.
type Endpoint struct {
	Row       EndpointRow
	Effective domain.Effective
	Hostname  string
}

func (s *Service) settings(ctx context.Context) (domain.CollabSettings, error) {
	var out domain.CollabSettings
	raw, err := s.Settings.Setting(ctx, domain.SettingCollabBaseDomain)
	if err != nil {
		return out, err
	}
	var base *string
	if err := json.Unmarshal(raw, &base); err != nil {
		return out, domain.Internal(err)
	}
	if base != nil {
		out.BaseDomain = *base
	}
	raw, err = s.Settings.Setting(ctx, domain.SettingCollabPublicEndpoints)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.PublicEndpoints); err != nil {
		return out, domain.Internal(err)
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, c domain.TeamCollaboration) (View, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return View{}, err
	}
	return View{Collab: c, Settings: set, Effective: c.ComputeEffective(set), Hostname: c.Hostname(set)}, nil
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
	return map[string]any{"agentId": c.AgentID, "webUiEnabled": c.WebUIEnabled, "restApiEnabled": c.RESTAPIEnabled,
		"registrationMode": string(c.Mode)}
}

// Get implements GetTeamCollaboration.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string) (View, error) {
	if err := s.check(ctx, c, authz.CollaborationRead, "GetTeamCollaboration", teamID); err != nil {
		return View{}, err
	}
	var col domain.TeamCollaboration
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, ok, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if !ok {
			return domain.CollaborationAgentMissing(false)
		}
		col = cur
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, col)
}

// Put implements PutTeamCollaboration. expect is nil when If-Match is absent (it is optional here).
// Switching a surface on is checked against the guardrail and the service settings; switching off is
// always accepted. One transaction writes the row, the audit record and collaboration.updated.
func (s *Service) Put(ctx context.Context, c acting.Caller, teamID string, p domain.PutCollaborationParams, expect *domain.Version) (View, error) {
	if err := s.check(ctx, c, authz.CollaborationManage, "PutTeamCollaboration", teamID); err != nil {
		return View{}, err
	}
	set, err := s.settings(ctx)
	if err != nil {
		return View{}, err
	}
	var out domain.TeamCollaboration
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, ok, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !ok {
			return domain.CollaborationAgentMissing(true)
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		ready := cur.Status.Phase == domain.PhaseReady
		if err := cur.ValidatePut(p, set, ready); err != nil {
			return err
		}
		mode := cur.Mode
		if p.Mode != "" {
			mode = p.Mode
		}
		changes := map[string]any{}
		change := func(name string, from, to any) {
			if from != to {
				changes[name] = map[string]any{"from": from, "to": to}
			}
		}
		change("webUiEnabled", cur.WebUIEnabled, p.WebUIEnabled)
		change("restApiEnabled", cur.RESTAPIEnabled, p.RESTAPIEnabled)
		change("registrationMode", string(cur.Mode), string(mode))
		if err := s.Repo.Update(ctx, tx, teamID, p.WebUIEnabled, p.RESTAPIEnabled, mode); err != nil {
			return err
		}
		next, _, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamCollaboration", kindCollab, teamID, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventCollaborationUpdated, teamID, kindCollab, teamID,
			map[string]any{"teamId": teamID, "agentId": cur.AgentID, "changes": changes}); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, out)
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
			return domain.CollaborationAgentMissing(false)
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
			return domain.CollaborationAgentMissing(true)
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
		case ag.Role == domain.AgentRoleCollaboration:
			return domain.Validation(domain.FieldError{Field: "agentId", Reason: "the collaboration agent cannot be connected to itself"})
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
			return domain.CollaborationAgentMissing(true)
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

// Endpoints implements ListCollaborationEndpoints (service admins; the route's permission is
// admin:settings). effective and the hostname are computed from the settings.
func (s *Service) Endpoints(ctx context.Context, c acting.Caller, f EndpointFilter, p acting.Page) ([]Endpoint, string, error) {
	if d := authz.Authorize(c.Principal, authz.AdminSettings, authz.Resource{Scope: authz.ScopeService}); !d.Allowed() {
		return nil, "", s.Denied(ctx, c, "ListCollaborationEndpoints", "", authz.AdminSettings)
	}
	set, err := s.settings(ctx)
	if err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []EndpointRow
	err = s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.Endpoints(ctx, tx, f, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r EndpointRow) string { return r.Collab.TeamID })
	out := make([]Endpoint, 0, len(page))
	for _, r := range page {
		out = append(out, Endpoint{Row: r, Effective: r.Collab.ComputeEffective(set), Hostname: r.Collab.Hostname(set)})
	}
	return out, next, nil
}
