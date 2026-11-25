package mcpcatalog

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

var mcpOutcomes = []string{"ok", "tool_error", "protocol_error", "refused", "upstream_unavailable", "timeout", "cancelled"}

// EventCursor is the keyset cursor of a ledger row: its time and id.
func EventCursor(e domain.McpEventRecord) string {
	return e.At.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(e.ID, 10)
}

// window fills an empty range with the last 24 hours and checks it.
func (s *Service) window(f *domain.McpEventFilter) error {
	if f.To.IsZero() {
		f.To = s.Now()
	}
	if f.From.IsZero() {
		f.From = f.To.Add(-24 * time.Hour)
	}
	if !f.To.After(f.From) {
		return domain.Validation(domain.FieldError{Field: "to", Reason: "must be after from"})
	}
	if f.Outcome != "" && !slices.Contains(mcpOutcomes, f.Outcome) {
		return domain.Validation(domain.FieldError{Field: "outcome", Reason: "unknown outcome"})
	}
	return nil
}

func (s *Service) events(ctx context.Context, c acting.Caller, f domain.McpEventFilter, service bool, p acting.Page) ([]domain.McpEventRecord, string, error) {
	if err := s.window(&f); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.McpEventRecord
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListEvents(ctx, tx, f, service, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, EventCursor)
	if page == nil {
		page = []domain.McpEventRecord{}
	}
	return page, next, nil
}

// ListEvents implements ListMcpEvents: the ledger of every team, for service admins and security operators.
func (s *Service) ListEvents(ctx context.Context, c acting.Caller, f domain.McpEventFilter, p acting.Page) ([]domain.McpEventRecord, string, error) {
	if err := s.checkService(ctx, c, "ListMcpEvents", authz.McpEventsRead); err != nil {
		return nil, "", err
	}
	return s.events(ctx, c, f, true, p)
}

// ListTeamEvents implements ListTeamMcpEvents: one team's ledger.
func (s *Service) ListTeamEvents(ctx context.Context, c acting.Caller, teamID string, f domain.McpEventFilter, p acting.Page) ([]domain.McpEventRecord, string, error) {
	if err := s.checkTeam(ctx, c, "ListTeamMcpEvents", teamID, authz.McpEventsRead); err != nil {
		return nil, "", err
	}
	f.TeamID = teamID
	return s.events(ctx, c, f, false, p)
}

func (s *Service) usage(ctx context.Context, c acting.Caller, q domain.McpUsageQuery) ([]domain.McpUsageRow, domain.McpUsageQuery, error) {
	if q.To.IsZero() {
		q.To = s.Now()
	}
	if q.From.IsZero() {
		q.From = q.To.AddDate(0, 0, -30)
	}
	if q.GroupBy == "" {
		q.GroupBy = "server"
	}
	if q.View == "" {
		q.View = "usage"
	}
	if q.View == "unused" && q.WindowDays == 0 {
		q.WindowDays = 30
	}
	if err := q.Validate(); err != nil {
		return nil, q, err
	}
	var rows []domain.McpUsageRow
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.UsageReport(ctx, tx, q)
		return err
	})
	if rows == nil {
		rows = []domain.McpUsageRow{}
	}
	return rows, q, err
}

// Usage implements GetMcpUsageReport.
func (s *Service) Usage(ctx context.Context, c acting.Caller, q domain.McpUsageQuery) ([]domain.McpUsageRow, domain.McpUsageQuery, error) {
	if err := s.checkService(ctx, c, "GetMcpUsageReport", authz.McpEventsRead); err != nil {
		return nil, q, err
	}
	q.TeamID = ""
	return s.usage(ctx, c, q)
}

// TeamUsage implements GetTeamMcpUsageReport (no team grouping, no adoption view).
func (s *Service) TeamUsage(ctx context.Context, c acting.Caller, teamID string, q domain.McpUsageQuery) ([]domain.McpUsageRow, domain.McpUsageQuery, error) {
	if err := s.checkTeam(ctx, c, "GetTeamMcpUsageReport", teamID, authz.McpEventsRead); err != nil {
		return nil, q, err
	}
	q.TeamID = teamID
	return s.usage(ctx, c, q)
}

func captureData(x domain.McpQueryCapture) map[string]any {
	return map[string]any{"captureId": x.ID, "startedBy": x.StartedBy, "until": x.Until.UTC().Format(time.RFC3339), "reason": x.Reason}
}

// CaptureResult is a capture with the replayed response of an idempotent start.
type CaptureResult struct {
	Capture domain.McpQueryCapture
	Replay  *port.StoredResponse
}

// StartCapture implements StartMcpQueryCapture: a window of at most mcp.query_capture_max_days in which afs_search
// query text is recorded (CD-50). One window at a time (409 capture_active).
func (s *Service) StartCapture(ctx context.Context, c acting.Caller, days int, reason string, idem *acting.Idem) (CaptureResult, error) {
	const action = "StartMcpQueryCapture"
	if err := s.checkService(ctx, c, action, authz.McpSettingsWrite); err != nil {
		return CaptureResult{}, err
	}
	var res CaptureResult
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		res = CaptureResult{}
		r, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || r != nil {
			res.Replay = r
			return err
		}
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		if err := domain.ValidateCaptureWindow(days, st.CaptureMaxDays, reason); err != nil {
			return err
		}
		open, err := s.Repo.OpenCapture(ctx, tx)
		if err != nil {
			return err
		}
		if open != nil {
			return domain.Conflict(domain.ReasonCaptureActive, "a capture window is open until "+open.Until.UTC().Format(time.RFC3339),
				map[string]any{"captureId": open.ID})
		}
		id, err := s.newID(domain.PrefixMcpCapture)
		if err != nil {
			return err
		}
		now := s.Now()
		x := domain.McpQueryCapture{ID: id, StartedBy: c.Session().Actor, StartedAt: now, Until: now.AddDate(0, 0, days), Reason: reason}
		if err := s.Repo.InsertCapture(ctx, tx, x); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindCapture, id, "", nil, captureData(x))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.McpCaptureEvent(true), "", kindCapture, id, captureData(x)); err != nil {
			return err
		}
		res.Capture = x
		return idem.Complete(ctx, s.Idem, tx, x)
	})
	return res, err
}

// StopCapture implements StopMcpQueryCapture. Stopping a window that had already expired records it as stopped at
// its end and emits nothing (the reconciler announced it, §4.12).
func (s *Service) StopCapture(ctx context.Context, c acting.Caller) (domain.McpQueryCapture, error) {
	const action = "StopMcpQueryCapture"
	if err := s.checkService(ctx, c, action, authz.McpSettingsWrite); err != nil {
		return domain.McpQueryCapture{}, err
	}
	var out domain.McpQueryCapture
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		x, err := s.Repo.LatestCapture(ctx, tx)
		if err != nil {
			return err
		}
		if x == nil {
			return domain.NotFound("query capture")
		}
		now := s.Now()
		expired := !x.Until.After(now)
		at, by := now, c.Session().Actor
		if expired {
			at, by = x.Until, "system:expired"
		}
		if err := s.Repo.StopCapture(ctx, tx, x.ID, at, by); err != nil {
			return err
		}
		before := captureData(*x)
		x.StoppedAt, x.StoppedBy = &at, by
		after := captureData(*x)
		after["stoppedAt"], after["stoppedBy"] = at.UTC().Format(time.RFC3339), by
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindCapture, x.ID, "", before, after)); err != nil {
			return err
		}
		out = *x
		if expired {
			return nil
		}
		return s.Emit(ctx, tx, c, domain.McpCaptureEvent(false), "", kindCapture, x.ID, captureData(*x))
	})
	return out, err
}

// GetCapture implements GetMcpQueryCapture: the open window, or nil.
func (s *Service) GetCapture(ctx context.Context, c acting.Caller) (*domain.McpQueryCapture, error) {
	if err := s.checkService(ctx, c, "GetMcpQueryCapture", authz.McpEventsRead); err != nil {
		return nil, err
	}
	var out *domain.McpQueryCapture
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.OpenCapture(ctx, tx)
		return err
	})
	return out, err
}

// FederatedView is a federated identity with the issuer and subject a target IdP trusts.
type FederatedView struct {
	Identity domain.FederatedIdentityRecord
	Issuer   string
	Subject  string
}

func fedKind(kind string) (table string, readPerm authz.Permission, list, put, del string) {
	if kind == "pool" {
		return kindPoolFI, authz.PoolsRead, "ListPoolFederatedIdentities", "PutPoolFederatedIdentity", "DeletePoolFederatedIdentity"
	}
	return kindAgentFI, authz.AgentsRead, "ListAgentFederatedIdentities", "PutAgentFederatedIdentity", "DeleteAgentFederatedIdentity"
}

func (s *Service) fedView(ns, account string, f domain.FederatedIdentityRecord) FederatedView {
	return FederatedView{Identity: f, Issuer: s.ClusterIssuer, Subject: domain.McpServiceAccountSubject(ns, account)}
}

// ListFederated implements ListAgentFederatedIdentities and ListPoolFederatedIdentities.
func (s *Service) ListFederated(ctx context.Context, c acting.Caller, teamID, kind, subjectID string) ([]FederatedView, error) {
	_, perm, action, _, _ := fedKind(kind)
	if err := s.checkTeam(ctx, c, action, teamID, perm); err != nil {
		return nil, err
	}
	var out []FederatedView
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		ns, account, err := s.Repo.SubjectAccount(ctx, tx, teamID, kind, subjectID)
		if err != nil {
			return err
		}
		rows, err := s.Repo.ListFederated(ctx, tx, kind, teamID, subjectID)
		if err != nil {
			return err
		}
		out = make([]FederatedView, 0, len(rows))
		for _, f := range rows {
			out = append(out, s.fedView(ns, account, f))
		}
		return nil
	})
	return out, err
}

func fedData(teamID, kind, subjectID string, f domain.FederatedIdentity, change string) map[string]any {
	m := map[string]any{"teamId": teamID, "name": f.Name, "audience": f.Audience, "change": change}
	if kind == "pool" {
		m["poolId"] = subjectID
	} else {
		m["agentId"] = subjectID
	}
	return m
}

func fedAudit(f domain.FederatedIdentity) map[string]any {
	return map[string]any{"name": f.Name, "audience": f.Audience, "expirationSeconds": f.ExpirationSeconds, "filePath": f.FilePath, "envVar": f.EnvVar}
}

// PutFederated implements PutAgentFederatedIdentity and PutPoolFederatedIdentity: at most 10 per agent or pool,
// the audience from federation.allowed_audiences and never a reserved one.
func (s *Service) PutFederated(ctx context.Context, c acting.Caller, teamID, kind, subjectID string, in domain.FederatedIdentity, expect *domain.Version) (FederatedView, error) {
	table, _, _, action, _ := fedKind(kind)
	if err := s.checkTeam(ctx, c, action, teamID, authz.AgentFederatedIdentitiesWrite); err != nil {
		return FederatedView{}, err
	}
	var out FederatedView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		ns, account, err := s.Repo.SubjectAccount(ctx, tx, teamID, kind, subjectID)
		if err != nil {
			return err
		}
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.Repo.GetFederated(ctx, tx, kind, teamID, subjectID, in.Name)
		if err != nil {
			return err
		}
		existing := 0
		if cur == nil {
			all, err := s.Repo.ListFederated(ctx, tx, kind, teamID, subjectID)
			if err != nil {
				return err
			}
			existing = len(all)
		}
		in = in.WithDefaults()
		if err := in.Validate(st.AllowedAudiences, existing); err != nil {
			return err
		}
		rec := domain.FederatedIdentityRecord{FederatedIdentity: in, TeamID: teamID, SubjectID: subjectID}
		var before any
		if cur != nil {
			if expect != nil {
				if err := domain.CheckVersion(cur.Version, *expect); err != nil {
					return err
				}
			}
			rec.ID = cur.ID
			before = fedAudit(cur.FederatedIdentity)
			if err := s.Repo.UpdateFederated(ctx, tx, kind, rec, cur.Version); err != nil {
				return err
			}
		} else {
			prefix := domain.PrefixAgentFederated
			if kind == "pool" {
				prefix = domain.PrefixPoolFederated
			}
			if rec.ID, err = s.newID(prefix); err != nil {
				return err
			}
			if err := s.Repo.InsertFederated(ctx, tx, kind, rec); err != nil {
				return err
			}
		}
		stored, err := s.Repo.GetFederated(ctx, tx, kind, teamID, subjectID, in.Name)
		if err != nil {
			return err
		}
		if stored == nil {
			return domain.NotFound("federated identity")
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, table, stored.ID, teamID, before, fedAudit(stored.FederatedIdentity))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.FederatedIdentityEvent, teamID, table, stored.ID, fedData(teamID, kind, subjectID, stored.FederatedIdentity, "put")); err != nil {
			return err
		}
		out = s.fedView(ns, account, *stored)
		return nil
	})
	return out, err
}

// DeleteFederated implements DeleteAgentFederatedIdentity and DeletePoolFederatedIdentity.
func (s *Service) DeleteFederated(ctx context.Context, c acting.Caller, teamID, kind, subjectID, name string, expect domain.Version) error {
	table, _, _, _, action := fedKind(kind)
	if err := s.checkTeam(ctx, c, action, teamID, authz.AgentFederatedIdentitiesWrite); err != nil {
		return err
	}
	return s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetFederated(ctx, tx, kind, teamID, subjectID, name)
		if err != nil {
			return err
		}
		if cur == nil {
			return domain.NotFound("federated identity")
		}
		if err := s.Repo.DeleteFederated(ctx, tx, kind, teamID, subjectID, name, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, table, cur.ID, teamID, fedAudit(cur.FederatedIdentity), nil)); err != nil {
			return err
		}
		return s.Emit(ctx, tx, c, domain.FederatedIdentityEvent, teamID, table, cur.ID, fedData(teamID, kind, subjectID, cur.FederatedIdentity, "delete"))
	})
}
