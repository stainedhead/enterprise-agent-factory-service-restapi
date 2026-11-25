// Package agents implements the agent operations of API contract §4.4 (M2-B). The API writes the
// desired state of an agent (one pod, no replica count) and the reconciler acts on it: a delete
// only marks the agent, pause, resume and restart change its state or generation, and an agent
// whose template is disabled later is not touched here (RA-US-4.5).
package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindAgent = "agents"
	// StatusKind is the resource_kind the reconciler writes agent status under (engine.KindAgent) and
	// the kind GetAgentStatus reports.
	StatusKind = "agent"
)

// TemplateInfo is a catalog agent template as the checks need it.
type TemplateInfo struct {
	ID             string
	Name           string
	Enabled        bool
	AllowedDigests []string
	Capabilities   []string
}

// SecretInfo says who owns a secret: an agent, a pool, or neither (team shared).
type SecretInfo struct {
	AgentID   string
	PoolID    string
	ManagedBy string
}

// Repository is the agent store, plus the catalog reads CreateAgent checks against.
type Repository interface {
	// List returns the team's agents that are not deleted, with id greater than after, in id order. A
	// non-empty role keeps only agents of that role.
	List(ctx context.Context, tx port.Tx, teamID string, role domain.AgentRole, after string, limit int) ([]domain.Agent, error)
	// Get returns one agent of the team that is not deleted; NotFound otherwise. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Agent, error)
	// Insert stores a new agent, its bindings and the catalog template reference.
	Insert(ctx context.Context, tx port.Tx, a domain.Agent, templateID string) error
	// Update stores the agent's spec, state, generation and bindings. Template and image are written
	// only when templateChanged, so the database's template guard does not run for any other change
	// (RA-US-4.5). PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, a domain.Agent, templateID string, templateChanged bool, expect domain.Version) error
	// Template returns the catalog template of that name.
	Template(ctx context.Context, tx port.Tx, name string) (TemplateInfo, bool, error)
	// ModelEnabled reports whether the model is enabled for the service and allowed for the team.
	ModelEnabled(ctx context.Context, tx port.Tx, teamID, modelID string) (bool, error)
	// ModelSets returns the service, team and callable model sets the models of an agent are checked
	// against (MOD-3), and the default model setting.
	ModelSets(ctx context.Context, tx port.Tx, teamID string) (domain.ModelSets, error)
	// EffectiveModels fills Models.Effective of each agent (service ∩ team ∩ agent ∩ callable).
	EffectiveModels(ctx context.Context, tx port.Tx, teamID string, list []domain.Agent) error
	// StorageClassEnabled reports whether the storage class exists and is offered.
	StorageClassEnabled(ctx context.Context, tx port.Tx, name string) (bool, error)
	// Secrets returns the owners of the team's secrets with these ids; unknown ids are absent.
	Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]SecretInfo, error)
	// Snapshots lists the agent's snapshots, newest id order, with id less than after (or all when
	// after is empty).
	Snapshots(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.Snapshot, error)
	// InsertSnapshot stores a manual snapshot request.
	InsertSnapshot(ctx context.Context, tx port.Tx, teamID string, s domain.Snapshot) error

	// CollaborationMode returns the team's tool and registration mode; ok is false when the team has no
	// orchestration agent (a 1.0.1 team the backfill has not reached).
	CollaborationMode(ctx context.Context, tx port.Tx, teamID string) (tool domain.CollabTool, mode domain.RegistrationMode, ok bool, err error)
	// SetCollaboration inserts or updates a worker's connection to Paperclip.
	SetCollaboration(ctx context.Context, tx port.Tx, teamID, agentID string, connect bool, paperclipAgentID *string) error
	// SecretPath returns afs.secret_path of an agent-scoped secret.
	SecretPath(ctx context.Context, tx port.Tx, teamID, agentID, name string) (string, error)
	// InsertBootSecret stores an AFS-managed secret row of the orchestration agent and binds it to the
	// agent as the environment variable env.
	InsertBootSecret(ctx context.Context, tx port.Tx, s domain.Secret, env string) error
	// ManagedBindings returns the agent's bindings whose secret AFS manages.
	ManagedBindings(ctx context.Context, tx port.Tx, teamID, agentID string) ([]domain.SecretBinding, error)
}

// BootCreated is a secret that now exists in Secrets Manager.
type BootCreated struct {
	ARN       string
	VersionID string
}

// BootWriter is the Secrets Manager write path for the orchestration agent's boot secrets (the same
// adapter as the secrets use case). CreateTeam and ChangeCollaborationTool use it.
type BootWriter interface {
	Create(ctx context.Context, name string, value domain.SecretValue, token string, tags map[string]string) (BootCreated, error)
	ForceDelete(ctx context.Context, arn string) error
	TagOrphan(ctx context.Context, arn string) error
}

// guard is the validator CreateAgent and UpdateAgent share with RestoreRevision (FR-R01).
func (s *Service) guard() Guard { return Guard{Src: s.Repo} }

// Service is the agent use case set.
type Service struct {
	acting.Deps
	Repo Repository
	// Boot writes the boot secrets of an orchestration agent (CreateBootSecrets); nil refuses a tool that
	// has boot secrets.
	Boot BootWriter
	// Rand is the random source of the boot secrets (crypto/rand.Reader in production).
	Rand io.Reader
	// OnOrphan is called when a compensating delete failed (afs_api_orphan_secrets_total). Optional.
	OnOrphan func()
	// Mcp stores the agents' MCP access (1.3.0). Nil leaves reads without mcp and refuses a body that sets it.
	Mcp mcpaccess.Store
}

// fill completes reads: the effective models and, when the MCP store is wired, the mcp member.
func (s *Service) fill(ctx context.Context, tx port.Tx, teamID string, list []domain.Agent) error {
	if err := s.Repo.EffectiveModels(ctx, tx, teamID, list); err != nil {
		return err
	}
	if s.Mcp == nil || len(list) == 0 {
		for i := range list {
			list[i].Mcp = nil
		}
		return nil
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	m, err := mcpaccess.Fill(ctx, tx, s.Mcp, teamID, mcpaccess.KindAgent, ids)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Mcp = m[list[i].ID]
	}
	return nil
}

// writeMcp stores the mcp member of a create or update and adds it to the audit records.
func (s *Service) writeMcp(ctx context.Context, tx port.Tx, teamID, id string, p *domain.McpAccessPatch, before, after map[string]any) error {
	if p == nil {
		return nil
	}
	if s.Mcp == nil {
		return mcpaccess.ErrNoStore
	}
	b, a, err := mcpaccess.Write(ctx, tx, s.Mcp, s.IDs, teamID, mcpaccess.KindAgent, id, *p)
	if err != nil {
		return err
	}
	if before != nil {
		before["mcp"] = b
	}
	after["mcp"] = a
	return nil
}

// Result is an agent with the replayed response of an idempotent call.
type Result struct {
	Agent  domain.Agent
	Replay *port.StoredResponse
}

// SnapshotResult is a snapshot with the replayed response of an idempotent create.
type SnapshotResult struct {
	Snapshot domain.Snapshot
	Replay   *port.StoredResponse
}

func auditOf(a domain.Agent) map[string]any {
	b := make([]map[string]string, 0, len(a.Bindings))
	for _, x := range a.Bindings {
		b = append(b, map[string]string{"secretId": x.SecretID, "delivery": x.Delivery, "target": x.Target})
	}
	return map[string]any{"name": string(a.Name), "description": a.Description, "template": a.Template, "image": string(a.Image),
		"role": string(a.Role), "modelId": a.ModelID, "models": modelsAudit(a.Models), "resources": a.Resources, "iamIdentity": a.IAMIdentity, "state": a.State, "generation": a.Generation,
		"volume": map[string]any{"sizeGiB": a.Volume.SizeGiB, "storageClass": a.Volume.StorageClass, "onDelete": a.Volume.OnDelete,
			"snapshotSchedule": a.Volume.SnapshotSchedule, "snapshotRetain": a.Volume.SnapshotRetain},
		"secretBindings": b}
}

func modelsAudit(m domain.AgentModels) map[string]any { return m.Audit() }

// emit publishes an agent event; unlike Deps.Emit it sets agentId in the detail.
func (s *Service) emit(ctx context.Context, tx port.Tx, c acting.Caller, typ domain.EventType, a domain.Agent, data map[string]any) error {
	eid, err := s.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), s.Clock.Now(), c.Session().Actor, a.TeamID, kindAgent, a.ID, data)
	if err != nil {
		return err
	}
	ev.Detail.AgentID = a.ID
	return s.Outbox.Publish(ctx, tx, ev)
}

// List implements ListAgents.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, role domain.AgentRole, p acting.Page) ([]domain.Agent, string, error) {
	p = p.Normalized()
	var rows []domain.Agent
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		if rows, err = s.Repo.List(ctx, tx, teamID, role.Normalize(), p.Cursor, p.Limit+1); err != nil {
			return err
		}
		return s.fill(ctx, tx, teamID, rows)
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.Agent) string { return a.ID })
	for i := range page {
		page[i] = page[i].WithDeletingStatus()
	}
	return page, next, nil
}

// Get implements GetAgent and GetAgentStatus.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID, id string) (domain.Agent, error) {
	var a domain.Agent
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		if a, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		one := []domain.Agent{a}
		if err = s.fill(ctx, tx, teamID, one); err != nil {
			return err
		}
		a = one[0]
		return nil
	})
	return a.WithDeletingStatus(), err
}

// Create implements CreateAgent: the model must be enabled for the team, the storage class offered,
// the template known, enabled and listing the image digest, and no binding may name another
// agent's secret. One transaction writes the row, the audit record and agent.created.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewAgentParams, idem *acting.Idem) (Result, error) {
	var res Result
	var created []string
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		created = nil
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixAgent)
		if err != nil {
			return err
		}
		a, err := domain.NewAgent(id.String(), teamID, in, s.Now())
		if err != nil {
			return err
		}
		sets, err := s.Repo.ModelSets(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if a.Models, err = domain.ResolveNewModels(in.Models, in.ModelID, sets); err != nil {
			return err
		}
		a.ModelID = a.Models.DefaultRef()
		if err := s.guard().CheckStorageClass(ctx, tx, a.Volume.StorageClass); err != nil {
			return err
		}
		tplID, err := s.guard().CheckTemplate(ctx, tx, a.Template, a.Image, a.Role)
		if err != nil {
			return err
		}
		if err := s.guard().CheckBindings(ctx, tx, teamID, a.ID, a.Bindings); err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, a, tplID); err != nil {
			return err
		}
		if err := s.connectNew(ctx, tx, teamID, a.ID, in.Collaboration); err != nil {
			return err
		}
		after := auditOf(a)
		if err := s.writeMcp(ctx, tx, teamID, a.ID, in.Mcp, nil, after); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgent", kindAgent, a.ID, teamID, nil, after)); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentCreated, a, map[string]any{"name": string(a.Name), "template": a.Template}); err != nil {
			return err
		}
		if res.Agent, err = s.Repo.Get(ctx, tx, teamID, a.ID, false); err != nil {
			return err
		}
		one := []domain.Agent{res.Agent}
		if err = s.fill(ctx, tx, teamID, one); err != nil {
			return err
		}
		res.Agent = one[0]
		return idem.Complete(ctx, s.Idem, tx, res.Agent)
	})
	if err != nil {
		s.compensate(ctx, created)
	}
	return res, err
}

var errNoBootWriter = errors.New("agents: no Secrets Manager writer for the boot secrets")

// CreateBootSecrets generates the tool's boot secrets (32 random bytes each, hex) and writes them to
// Secrets Manager, then stores the AFS-managed rows bound to the orchestration agent (COL-7). created
// collects every ARN written, so a failure anywhere in the request deletes them (Compensate). A Secrets
// Manager failure is a 503. A tool with no boot secrets (Buzz) writes nothing.
func (s *Service) CreateBootSecrets(ctx context.Context, tx port.Tx, c acting.Caller, teamID, agentID string, tool domain.CollabTool, idem *acting.Idem, created *[]string) error {
	boots := tool.BootSecrets()
	if len(boots) == 0 {
		return nil
	}
	if s.Boot == nil || s.Rand == nil {
		return domain.Unavailable("Secrets Manager", errNoBootWriter)
	}
	for _, b := range boots {
		path, err := s.Repo.SecretPath(ctx, tx, teamID, agentID, b.Name)
		if err != nil {
			return err
		}
		val, err := domain.GenerateSecretValue(s.Rand, 32)
		if err != nil {
			return err
		}
		cr, err := s.Boot.Create(ctx, path, val, bootToken(c, idem, teamID, path), map[string]string{"afs-team-id": teamID, "afs-agent-id": agentID})
		if err != nil {
			e := domain.Unavailable("Secrets Manager", err)
			e.RetryAfterSeconds = 1
			return e
		}
		*created = append(*created, cr.ARN)
		sid, err := s.IDs.NewID(domain.PrefixSecret)
		if err != nil {
			return err
		}
		now := s.Now()
		sec := domain.Secret{ID: sid.String(), TeamID: teamID, Scope: domain.SecretScopeAgent, AgentID: agentID, Name: b.Name,
			Description: tool.DisplayName() + " boot secret, generated by AFS", Path: path, ARN: cr.ARN, VersionID: cr.VersionID,
			LastChangedAt: &now, ManagedBy: domain.ManagedByAFS}
		if err := s.Repo.InsertBootSecret(ctx, tx, sec, b.Env); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateSecret", "secrets", sec.ID, teamID, nil,
			map[string]any{"scope": string(sec.Scope), "agentId": agentID, "name": sec.Name, "managedBy": string(sec.ManagedBy), "path": path})); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventSecretCreated, teamID, "secrets", sec.ID,
			map[string]any{"secretId": sec.ID, "name": sec.Name, "scope": string(sec.Scope)}); err != nil {
			return err
		}
	}
	return nil
}

// Compensate deletes the Secrets Manager secrets a failed request wrote; a delete that fails tags the
// secret for the reconciler's orphan sweep (RC-27).
func (s *Service) Compensate(ctx context.Context, arns []string) { s.compensate(ctx, arns) }

func bootToken(c acting.Caller, idem *acting.Idem, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(c.Principal.ID))
	if idem != nil {
		h.Write([]byte("|idem:" + idem.Key.Key))
	} else {
		h.Write([]byte("|req:" + c.RequestID))
	}
	for _, p := range parts {
		h.Write([]byte("|" + p))
	}
	return hex.EncodeToString(h.Sum(nil))[:48]
}

// compensate deletes the Secrets Manager secrets a failed CreateAgent wrote; a delete that fails tags
// the secret for the reconciler's orphan sweep (RC-27).
func (s *Service) compensate(ctx context.Context, arns []string) {
	ctx = context.WithoutCancel(ctx)
	for _, arn := range arns {
		if err := s.Boot.ForceDelete(ctx, arn); err == nil {
			continue
		}
		_ = s.Boot.TagOrphan(ctx, arn)
		if s.OnOrphan != nil {
			s.OnOrphan()
		}
	}
}

// connect records a worker's collaboration choice. The team must have an orchestration agent, and the
// Paperclip agent id is accepted only for Paperclip in manual registration (RA-18).
func (s *Service) connect(ctx context.Context, tx port.Tx, teamID, agentID string, in domain.CollaborationInput) error {
	tool, mode, ok, err := s.Repo.CollaborationMode(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.OrchestrationPending(true)
	}
	if in.PaperclipAgentID != "" && (mode == domain.RegistrationManaged || tool != domain.ToolPaperclip) {
		return domain.Validation(domain.FieldError{Field: "collaboration.paperclipAgentId", Reason: domain.ReasonManagedRegistration})
	}
	var pc *string
	if in.PaperclipAgentID != "" {
		pc = &in.PaperclipAgentID
	}
	return s.Repo.SetCollaboration(ctx, tx, teamID, agentID, in.Connect != nil && *in.Connect, pc)
}

// connectNew records the connection of a new worker: connect defaults to true because every team has an
// orchestration agent (COL-10 change). A team the backfill has not reached has none, so the default is
// false there; an explicit connect true with no agent is refused.
func (s *Service) connectNew(ctx context.Context, tx port.Tx, teamID, agentID string, in *domain.CollaborationInput) error {
	if in != nil && in.Connect != nil {
		return s.connect(ctx, tx, teamID, agentID, *in)
	}
	_, _, ok, err := s.Repo.CollaborationMode(ctx, tx, teamID)
	if err != nil || !ok {
		return err
	}
	t := true
	return s.connect(ctx, tx, teamID, agentID, domain.CollaborationInput{Connect: &t})
}

func changedFields(p domain.AgentPatch) []string {
	var f []string
	add := func(set bool, n string) {
		if set {
			f = append(f, n)
		}
	}
	add(p.Description != nil, "description")
	add(p.Template != nil, "template")
	add(p.Image != nil, "image")
	add(p.ModelID != nil, "modelId")
	add(p.Models.Present(), "models")
	add(p.Resources != nil, "resources")
	add(p.IAMIdentity != nil, "iamIdentity")
	add(p.Volume != nil, "volume")
	add(p.Bindings != nil, "secretBindings")
	add(p.Collaboration != nil, "collaboration")
	add(p.Mcp != nil, "mcp")
	return f
}

// Update implements UpdateAgent (If-Match required). Template, image, model and bindings are
// checked only when the request changes them, so an agent whose template was disabled later can
// still be edited in other fields (RA-US-4.5).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version, p domain.AgentPatch) (domain.Agent, error) {
	var out domain.Agent
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(p, s.Now())
		if err != nil {
			return err
		}
		tplChanged := next.Template != cur.Template || next.Image != cur.Image
		tplID := ""
		if tplChanged {
			if tplID, err = s.guard().CheckTemplate(ctx, tx, next.Template, next.Image, cur.Role); err != nil {
				return err
			}
		}
		next.Models = cur.Models
		if p.Models.Present() || p.ModelID != nil {
			sets, err := s.Repo.ModelSets(ctx, tx, teamID)
			if err != nil {
				return err
			}
			legacy := ""
			if p.ModelID != nil {
				legacy = *p.ModelID
			}
			if next.Models, err = cur.Models.ApplyModels(p.Models, legacy, sets); err != nil {
				return err
			}
			next.ModelID = next.Models.DefaultRef()
		}
		if p.Bindings != nil {
			if err := s.guard().CheckBindings(ctx, tx, teamID, id, next.Bindings); err != nil {
				return err
			}
			if cur.IsOrchestration() {
				// The boot bindings are AFS's: a replacement of the bindings keeps them.
				kept, err := s.Repo.ManagedBindings(ctx, tx, teamID, id)
				if err != nil {
					return err
				}
				for _, k := range kept {
					if !slices.ContainsFunc(next.Bindings, func(b domain.SecretBinding) bool { return b.Target == k.Target }) {
						next.Bindings = append(next.Bindings, k)
					}
				}
			}
		}
		if err := s.Repo.Update(ctx, tx, next, tplID, tplChanged, expect); err != nil {
			return err
		}
		if p.Collaboration != nil && p.Collaboration.Connect != nil {
			if err := s.connect(ctx, tx, teamID, id, *p.Collaboration); err != nil {
				return err
			}
		}
		before, after := auditOf(cur), auditOf(next)
		if err := s.writeMcp(ctx, tx, teamID, id, p.Mcp, before, after); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateAgent", kindAgent, id, teamID, before, after)); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentUpdated, next, map[string]any{"fields": changedFields(p)}); err != nil {
			return err
		}
		if out, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		one := []domain.Agent{out}
		if err = s.fill(ctx, tx, teamID, one); err != nil {
			return err
		}
		out = one[0]
		return nil
	})
	return out, err
}

// Delete implements DeleteAgent: the agent is marked deleting (status Deleting) and the reconciler
// runs the delete steps and finalizes it. Deleting a deleting agent changes nothing.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) (domain.Agent, error) {
	var out domain.Agent
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if cur.IsOrchestration() {
			// The orchestration agent is deleted only with its team (ORC-2); the refusal is audited as denied.
			_ = s.Audit.WriteDenied(context.WithoutCancel(ctx), c.Session(), port.AuditRecord{Action: "DeleteAgent", ResourceKind: kindAgent,
				ResourceID: id, TeamID: teamID, SourceIP: c.SourceIP, UserAgent: c.UserAgent,
				After: map[string]any{"reason": domain.ReasonOrchestrationAgentReq}})
			return domain.Conflict(domain.ReasonOrchestrationAgentReq, "the orchestration agent is deleted only with its team", nil)
		}
		next, changed, err := cur.MarkDeleting(s.Now())
		if err != nil {
			return err
		}
		if !changed {
			out = next
			return nil
		}
		if err := s.Repo.Update(ctx, tx, next, "", false, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("DeleteAgent", kindAgent, id, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentDeleted, next, map[string]any{"name": string(next.Name)}); err != nil {
			return err
		}
		if out, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		out = out.WithDeletingStatus()
		return nil
	})
	return out, err
}

type verb struct {
	action string
	event  domain.EventType
	apply  func(domain.Agent) (domain.Agent, bool, error)
}

func (s *Service) transition(ctx context.Context, c acting.Caller, teamID, id string, v verb, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		next, changed, err := v.apply(cur)
		if err != nil {
			return err
		}
		if changed {
			if err := s.Repo.Update(ctx, tx, next, "", false, cur.Version); err != nil {
				return err
			}
			if err := s.Audit.Write(ctx, tx, c.Record(v.action, kindAgent, id, teamID,
				map[string]any{"state": cur.State, "generation": cur.Generation}, map[string]any{"state": next.State, "generation": next.Generation})); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, c, v.event, next, map[string]any{"name": string(next.Name)}); err != nil {
				return err
			}
		}
		if res.Agent, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Agent)
	})
	return res, err
}

// Pause implements PauseAgent: the pod scales to zero, the volume stays.
func (s *Service) Pause(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"PauseAgent", domain.EventAgentPaused,
		func(a domain.Agent) (domain.Agent, bool, error) { return a.Pause(s.Now()) }}, idem)
}

// Resume implements ResumeAgent: the same volume comes back.
func (s *Service) Resume(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"ResumeAgent", domain.EventAgentResumed,
		func(a domain.Agent) (domain.Agent, bool, error) { return a.Resume(s.Now()) }}, idem)
}

// Restart implements RestartAgent: the generation rises and the reconciler starts a fresh pod.
func (s *Service) Restart(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"RestartAgent", domain.EventAgentRestarted,
		func(a domain.Agent) (domain.Agent, bool, error) {
			n, err := a.Restart(s.Now())
			return n, err == nil, err
		}}, idem)
}

// ListSnapshots implements ListAgentSnapshots, newest first.
func (s *Service) ListSnapshots(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.Snapshot, string, error) {
	p = p.Normalized()
	var rows []domain.Snapshot
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Get(ctx, tx, teamID, agentID, false); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.Snapshots(ctx, tx, teamID, agentID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.Snapshot) string { return x.ID })
	return page, next, nil
}

// CreateSnapshot implements CreateAgentSnapshot: it records a manual request the reconciler
// carries out. Only an enabled or paused agent can be snapshotted.
func (s *Service) CreateSnapshot(ctx context.Context, c acting.Caller, teamID, agentID, description string, idem *acting.Idem) (SnapshotResult, error) {
	if len([]rune(description)) > 200 {
		return SnapshotResult{}, domain.Validation(domain.FieldError{Field: "description", Reason: "must be at most 200 characters"})
	}
	var res SnapshotResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = SnapshotResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		a, err := s.Repo.Get(ctx, tx, teamID, agentID, true)
		if err != nil {
			return err
		}
		if a.State != domain.AgentEnabled && a.State != domain.AgentPaused {
			return domain.Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be snapshotted", nil)
		}
		id, err := s.IDs.NewID(domain.PrefixSnapshot)
		if err != nil {
			return err
		}
		snap := domain.Snapshot{ID: id.String(), AgentID: agentID, Reason: "manual", State: "requested", RequestedAt: s.Now(), RequestedBy: c.Session().Actor}
		if err := s.Repo.InsertSnapshot(ctx, tx, teamID, snap); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgentSnapshot", kindAgent, agentID, teamID, nil,
			map[string]any{"snapshotId": snap.ID, "reason": snap.Reason, "description": description})); err != nil {
			return err
		}
		res.Snapshot = snap
		return idem.Complete(ctx, s.Idem, tx, res.Snapshot)
	})
	return res, err
}
