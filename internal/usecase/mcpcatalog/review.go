package mcpcatalog

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// PrimitiveView is a primitive with, on team paths, the team's decision.
type PrimitiveView struct {
	Primitive domain.McpPrimitiveRecord
	Decision  *domain.McpDecisionView
}

// FieldDiff is one changed top-level member of a definition.
type FieldDiff struct {
	Field  string
	Before any
	After  any
}

// PrimitiveChange is a held change with what a reviewer needs (screen 49).
type PrimitiveChange struct {
	Primitive          PrimitiveView
	ApprovedDefinition json.RawMessage
	PendingDefinition  json.RawMessage
	Diff               []FieldDiff
	Checks             []domain.McpCheck
	CrossesRiskLine    bool
}

// ListPrimitives implements ListMcpPrimitives.
func (s *Service) ListPrimitives(ctx context.Context, c acting.Caller, serverID string, f PrimitiveFilter, p acting.Page) ([]PrimitiveView, string, error) {
	if err := s.checkService(ctx, c, "ListMcpPrimitives", authz.McpServersRead); err != nil {
		return nil, "", err
	}
	if err := validPrimitiveFilter(f); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var out []PrimitiveView
	var next string
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.GetServer(ctx, tx, serverID, false); err != nil {
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
			out = append(out, PrimitiveView{Primitive: x})
		}
		return nil
	})
	return out, next, err
}

func validPrimitiveFilter(f PrimitiveFilter) error {
	if f.Kind != "" && !mcp.Kind(f.Kind).Valid() {
		return domain.Validation(domain.FieldError{Field: "kind", Reason: "must be tool, resource, resource_template or prompt"})
	}
	if f.Status != "" && !slices.Contains([]string{domain.McpPrimitiveApproved, domain.McpPrimitivePending, domain.McpPrimitiveBlocked}, f.Status) {
		return domain.Validation(domain.FieldError{Field: "status", Reason: "must be approved, pending_review or blocked"})
	}
	return nil
}

// GetPrimitiveChange implements GetMcpPrimitiveChange: approved and pending definitions, the field diff, the
// definition checks and whether the change crosses a risk line.
func (s *Service) GetPrimitiveChange(ctx context.Context, c acting.Caller, serverID, primitiveID string) (PrimitiveChange, error) {
	if err := s.checkService(ctx, c, "GetMcpPrimitiveChange", authz.McpServersRead); err != nil {
		return PrimitiveChange{}, err
	}
	var out PrimitiveChange
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		srv, err := s.Repo.GetServer(ctx, tx, serverID, false)
		if err != nil {
			return err
		}
		p, err := s.Repo.GetPrimitive(ctx, tx, serverID, primitiveID, false)
		if err != nil {
			return err
		}
		out = buildChange(srv.Slug, p)
		return nil
	})
	return out, err
}

func buildChange(slug string, p domain.McpPrimitiveRecord) PrimitiveChange {
	ch := PrimitiveChange{Primitive: PrimitiveView{Primitive: p}}
	pending := p.PendingDefinition
	if p.ApprovedDigest != "" {
		ch.ApprovedDefinition = p.Definition
	}
	if len(pending) == 0 {
		pending = p.Definition
	}
	ch.PendingDefinition = pending
	var a, b map[string]any
	_ = json.Unmarshal(ch.ApprovedDefinition, &a)
	_ = json.Unmarshal(pending, &b)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	ch.Diff = []FieldDiff{}
	for _, k := range names {
		if mustJSON(a[k]) != mustJSON(b[k]) {
			ch.Diff = append(ch.Diff, FieldDiff{Field: k, Before: a[k], After: b[k]})
		}
	}
	var before *mcp.Annotations
	if ch.ApprovedDefinition != nil {
		before = annotationsOf(ch.ApprovedDefinition)
	}
	after := annotationsOf(pending)
	ch.CrossesRiskLine = mcp.CrossesRiskLine(before, *after, mcp.Kind(p.Kind))
	prim := toPrimitive(slug, p)
	ch.Checks = []domain.McpCheck{}
	for _, f := range mcp.CheckDefinitions([]mcp.Primitive{prim}, nil) {
		ch.Checks = append(ch.Checks, domain.McpCheck{Name: "description_injection", Outcome: domain.McpCheckFail, Detail: f.Check + ": " + f.Detail})
	}
	if len(ch.Checks) == 0 {
		ch.Checks = append(ch.Checks, domain.McpCheck{Name: "description_injection", Outcome: domain.McpCheckPass})
	}
	return ch
}

func annotationsOf(def json.RawMessage) *mcp.Annotations {
	var d struct {
		Annotations mcp.Annotations `json:"annotations"`
	}
	_ = json.Unmarshal(def, &d)
	return &d.Annotations
}

// decision is one primitive decision of a review.
type decision struct {
	PrimitiveID string
	Approve     bool
}

// PrimitiveDecision is a reviewer's decision on one primitive in a server review.
type PrimitiveDecision struct {
	PrimitiveID string
	Decision    string // approve or block
}

// decidePrimitive moves one primitive (approve: pending or blocked → approved; block: pending → blocked), writes
// its review record, audit and event. approver is team_admin or security.
func (s *Service) decidePrimitive(ctx context.Context, tx port.Tx, c acting.Caller, action string, srv domain.McpServerRecord,
	p domain.McpPrimitiveRecord, approve bool, approver, note string) (domain.McpPrimitiveRecord, error) {
	to := domain.McpPrimitiveBlocked
	if approve {
		to = domain.McpPrimitiveApproved
	}
	ok := (p.Status == domain.McpPrimitivePending) || (approve && p.Status == domain.McpPrimitiveBlocked)
	if !ok {
		return p, domain.Conflict(domain.ReasonInvalidReviewTransition, "the primitive is "+p.Status, map[string]any{"status": p.Status})
	}
	if err := s.Repo.DecidePrimitive(ctx, tx, p.ID, to); err != nil {
		return p, err
	}
	stored, err := s.Repo.GetPrimitive(ctx, tx, srv.ID, p.ID, false)
	if err != nil {
		return p, err
	}
	rid, err := s.newID(domain.PrefixMcpReview)
	if err != nil {
		return p, err
	}
	dec := domain.McpDecisionBlocked
	if approve {
		dec = domain.McpDecisionApproved
	}
	if err := s.Repo.InsertReview(ctx, tx, domain.McpReview{ID: rid, ServerID: srv.ID, TeamID: srv.TeamID, PrimitiveID: p.ID,
		RiskTier: srv.RiskTier, Approver: approver, Decision: dec, Note: note}); err != nil {
		return p, err
	}
	audit := func(x domain.McpPrimitiveRecord) map[string]any {
		return map[string]any{"id": x.ID, "serverId": x.ServerID, "kind": x.Kind, "name": x.Name, "status": x.Status, "digest": x.Digest}
	}
	if err := s.Audit.Write(ctx, tx, c.Record(action, kindPrimitive, p.ID, srv.TeamID, audit(p), audit(stored))); err != nil {
		return p, err
	}
	return stored, s.Emit(ctx, tx, c, domain.McpPrimitiveEvent(to), srv.TeamID, kindPrimitive, p.ID, map[string]any{
		"serverId": srv.ID, "primitiveId": p.ID, "digest": stored.Digest, "decidedBy": c.Session().Actor})
}

// primitiveDecision runs one primitive decision in its own transaction; allow refuses with a 403 when the caller
// may not decide this change.
func (s *Service) primitiveDecision(ctx context.Context, c acting.Caller, action, serverID, primitiveID, note string, approve bool,
	approver string, scope func(domain.McpServerRecord) error,
	allow func(st Settings, srv domain.McpServerRecord, p domain.McpPrimitiveRecord) error, idem *acting.Idem) (PrimitiveView, *port.StoredResponse, error) {
	if len(note) > domain.MaxMcpReasonLength {
		return PrimitiveView{}, nil, domain.Validation(domain.FieldError{Field: "note", Reason: "at most 500 characters"})
	}
	var out PrimitiveView
	var replay *port.StoredResponse
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || r != nil {
			replay = r
			return err
		}
		srv, err := s.Repo.GetServer(ctx, tx, serverID, true)
		if err != nil {
			return err
		}
		if err := scope(srv); err != nil {
			return err
		}
		p, err := s.Repo.GetPrimitive(ctx, tx, serverID, primitiveID, true)
		if err != nil {
			return err
		}
		if allow != nil {
			st, err := s.Repo.Settings(ctx, tx)
			if err != nil {
				return err
			}
			if err := allow(st, srv, p); err != nil {
				return err
			}
		}
		if note == "" {
			note = "decided without a note"
		}
		stored, err := s.decidePrimitive(ctx, tx, c, action, srv, p, approve, approver, note)
		if err != nil {
			return err
		}
		out = PrimitiveView{Primitive: stored}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, replay, err
}

// DecidePrimitive implements ApproveMcpPrimitive (approve) and RejectMcpPrimitive (block).
func (s *Service) DecidePrimitive(ctx context.Context, c acting.Caller, serverID, primitiveID, note string, approve bool, idem *acting.Idem) (PrimitiveView, *port.StoredResponse, error) {
	action := "RejectMcpPrimitive"
	if approve {
		action = "ApproveMcpPrimitive"
	}
	if err := s.checkService(ctx, c, action, authz.McpServersReview); err != nil {
		return PrimitiveView{}, nil, err
	}
	return s.primitiveDecision(ctx, c, action, serverID, primitiveID, note, approve, "security",
		func(domain.McpServerRecord) error { return nil }, nil, idem)
}

// ReviewInput is a server review decision: a required note and optional primitive decisions.
type ReviewInput struct {
	Note       string
	Primitives []PrimitiveDecision
}

func (in ReviewInput) validate() ([]decision, error) {
	if err := domain.ValidateMcpReason("note", in.Note); err != nil {
		return nil, err
	}
	if len(in.Primitives) > domain.MaxMcpPrimitivesPerServer {
		return nil, domain.Validation(domain.FieldError{Field: "primitives", Reason: "at most 2000"})
	}
	out := make([]decision, 0, len(in.Primitives))
	seen := map[string]bool{}
	for _, p := range in.Primitives {
		if p.Decision != "approve" && p.Decision != "block" {
			return nil, domain.Validation(domain.FieldError{Field: "primitives", Reason: "decision must be approve or block"})
		}
		if seen[p.PrimitiveID] {
			return nil, domain.Validation(domain.FieldError{Field: "primitives", Reason: "duplicate primitiveId " + p.PrimitiveID})
		}
		seen[p.PrimitiveID] = true
		out = append(out, decision{PrimitiveID: p.PrimitiveID, Approve: p.Decision == "approve"})
	}
	return out, nil
}

// review moves a pending server to approved, rejected or draft (changes requested), applies the primitive
// decisions and records the review.
func (s *Service) review(ctx context.Context, c acting.Caller, action, serverID string, in ReviewInput, to domain.McpReviewStatus,
	changes bool, approver string, scope func(domain.McpServerRecord) error,
	allow func(st Settings, srv domain.McpServerRecord) error, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	decs, err := in.validate()
	if err != nil {
		return ServerView{}, nil, err
	}
	var replay *port.StoredResponse
	t := transition{action: action, scope: scope,
		// An enterprise server has no submit step: a reviewer decides on its draft, which enters review first (the
		// database allows draft to pending_review only). A team server's draft is submitted by its team.
		before: func(ctx context.Context, tx port.Tx, cur domain.McpServerRecord) (domain.McpServerRecord, error) {
			if cur.IsTeam() || cur.ReviewStatus != domain.McpDraft || to == domain.McpDraft {
				return cur, nil
			}
			next := cur
			next.ReviewStatus = domain.McpPendingReview
			return s.store(ctx, tx, next)
		},
		change: func(st Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if allow != nil {
				if err := allow(st, cur); err != nil {
					return "", nil, err
				}
			}
			if cur.ReviewStatus != domain.McpPendingReview {
				return "", nil, domain.Conflict(domain.ReasonInvalidReviewTransition, "the server is "+string(cur.ReviewStatus)+", not pending_review",
					map[string]any{"reviewStatus": string(cur.ReviewStatus)})
			}
			next.ReviewStatus = to
			return domain.McpReviewEvent(to, changes), map[string]any{"riskTier": cur.RiskTier, "approver": approver,
				"decidedBy": c.Session().Actor, "note": in.Note}, nil
		},
		after: func(ctx context.Context, tx port.Tx, _ Settings, stored domain.McpServerRecord) (domain.McpServerRecord, error) {
			for _, d := range decs {
				p, err := s.Repo.GetPrimitive(ctx, tx, stored.ID, d.PrimitiveID, true)
				if err != nil {
					return stored, err
				}
				if _, err := s.decidePrimitive(ctx, tx, c, action, stored, p, d.Approve, approver, in.Note); err != nil {
					return stored, err
				}
			}
			dec := map[domain.McpReviewStatus]string{domain.McpApproved: domain.McpDecisionApproved, domain.McpRejected: domain.McpDecisionRejected,
				domain.McpDraft: domain.McpDecisionChangesRequested}[to]
			rid, err := s.newID(domain.PrefixMcpReview)
			if err != nil {
				return stored, err
			}
			return stored, s.Repo.InsertReview(ctx, tx, domain.McpReview{ID: rid, ServerID: stored.ID, TeamID: stored.TeamID, Checks: stored.Checks,
				RiskTier: stored.RiskTier, Approver: approver, Decision: dec, Note: in.Note})
		}}
	v, err := s.runIdem(ctx, c, serverID, nil, t, idem, &replay)
	return v, replay, err
}

// runIdem is run with an optional idempotency key around the transition.
func (s *Service) runIdem(ctx context.Context, c acting.Caller, id string, expect *domain.Version, t transition, idem *acting.Idem, replay **port.StoredResponse) (ServerView, error) {
	var out ServerView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		r, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || r != nil {
			*replay = r
			return err
		}
		if out, err = s.runIn(ctx, tx, c, id, expect, t); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return out, err
}

// Review implements ApproveMcpServer, RejectMcpServer and RequestMcpServerChanges on the service path.
func (s *Service) Review(ctx context.Context, c acting.Caller, serverID string, to domain.McpReviewStatus, in ReviewInput, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	action, changes := reviewAction(to, "")
	if err := s.checkService(ctx, c, action, authz.McpServersReview); err != nil {
		return ServerView{}, nil, err
	}
	return s.review(ctx, c, action, serverID, in, to, changes, "security", func(domain.McpServerRecord) error { return nil }, nil, idem)
}

func reviewAction(to domain.McpReviewStatus, team string) (string, bool) {
	switch to {
	case domain.McpApproved:
		return "Approve" + team + "McpServer", false
	case domain.McpRejected:
		return "Reject" + team + "McpServer", false
	}
	return "RequestMcpServerChanges", true
}

// ReviewQueueFilter filters ListMcpReviews.
type ReviewQueueFilter struct {
	Kind     string
	RiskTier string
	TeamID   string
	Approver string
}

// ListReviews implements ListMcpReviews: pending servers and held primitives of every team.
func (s *Service) ListReviews(ctx context.Context, c acting.Caller, f ReviewQueueFilter, p acting.Page) ([]domain.McpReviewQueueItem, string, error) {
	if err := s.checkService(ctx, c, "ListMcpReviews", authz.McpServersReview); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var items []domain.McpReviewQueueItem
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		all, err := s.Repo.ReviewQueue(ctx, tx)
		if err != nil {
			return err
		}
		for _, it := range all {
			if (f.Kind == "" || f.Kind == it.Kind) && (f.RiskTier == "" || f.RiskTier == it.RiskTier) &&
				(f.TeamID == "" || f.TeamID == it.TeamID) && (f.Approver == "" || f.Approver == it.Approver) {
				items = append(items, it)
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	key := func(it domain.McpReviewQueueItem) string { return it.ServerID + "/" + it.PrimitiveID }
	start := 0
	if p.Cursor != "" {
		for i, it := range items {
			if key(it) == p.Cursor {
				start = i + 1
				break
			}
		}
	}
	items = items[start:]
	page, next := acting.Slice(items, p.Limit, key)
	if page == nil {
		page = []domain.McpReviewQueueItem{}
	}
	return page, next, nil
}

// PolicyView is a server's rules at one level.
type PolicyView struct {
	ServerID         string
	Level            string
	TeamID           string
	Rules            []domain.McpPolicyRule
	DriftPolicy      string
	AllowSampling    bool
	TrustAnnotations bool
	Version          domain.Version
}

// PolicyInput is a PutMcpPolicy or PutTeamMcpPolicy body.
type PolicyInput struct {
	Rules            []domain.McpPolicyRule
	DriftPolicy      *string
	AllowSampling    *bool
	TrustAnnotations *bool
}

// replaceRules swaps a scope's rules and emits policy_changed with the counts.
func (s *Service) replaceRules(ctx context.Context, tx port.Tx, c acting.Caller, action string, srv domain.McpServerRecord, sc PolicyScope, rules []domain.McpPolicyRule) error {
	before, err := s.Repo.ListRules(ctx, tx, sc)
	if err != nil {
		return err
	}
	ids, err := s.newIDs(domain.PrefixMcpPolicy, len(rules))
	if err != nil {
		return err
	}
	if err := s.Repo.ReplaceRules(ctx, tx, sc, rules, ids); err != nil {
		return err
	}
	added, removed := domain.McpRuleDiff(before, rules)
	if err := s.Audit.Write(ctx, tx, c.Record(action, kindPolicy, srv.ID, sc.TeamID, map[string]any{"level": sc.Level, "rules": before},
		map[string]any{"level": sc.Level, "rules": rules})); err != nil {
		return err
	}
	data := map[string]any{"serverId": srv.ID, "level": sc.Level, "rules": map[string]any{"added": added, "removed": removed}}
	if sc.TeamID != "" {
		data["teamId"] = sc.TeamID
	}
	return s.Emit(ctx, tx, c, domain.McpPolicyEvent, sc.TeamID, kindPolicy, srv.ID, data)
}

// PutPolicy implements PutMcpPolicy: the service rules and the server's drift, sampling and annotation trust.
func (s *Service) PutPolicy(ctx context.Context, c acting.Caller, serverID string, in PolicyInput, expect *domain.Version) (PolicyView, error) {
	if err := s.checkService(ctx, c, "PutMcpPolicy", authz.McpServersWrite); err != nil {
		return PolicyView{}, err
	}
	if err := domain.ValidateMcpPolicyRules(in.Rules, "rules"); err != nil {
		return PolicyView{}, err
	}
	var out PolicyView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		cur, err := s.lockServer(ctx, tx, serverID, expect)
		if err != nil {
			return err
		}
		next := cur
		set(&next.DriftPolicy, in.DriftPolicy)
		set(&next.AllowSampling, in.AllowSampling)
		set(&next.TrustAnnotations, in.TrustAnnotations)
		if !slices.Contains([]string{domain.McpDriftHold, domain.McpDriftAutoCompatible}, next.DriftPolicy) {
			return domain.Validation(domain.FieldError{Field: "driftPolicy", Reason: "must be hold or auto_approve_compatible"})
		}
		stored := cur
		if next.DriftPolicy != cur.DriftPolicy || next.AllowSampling != cur.AllowSampling || next.TrustAnnotations != cur.TrustAnnotations {
			if stored, err = s.store(ctx, tx, next); err != nil {
				return err
			}
		}
		if err := s.replaceRules(ctx, tx, c, "PutMcpPolicy", stored, PolicyScope{ServerID: serverID, Level: "service"}, in.Rules); err != nil {
			return err
		}
		out = PolicyView{ServerID: serverID, Level: "service", Rules: in.Rules, DriftPolicy: stored.DriftPolicy,
			AllowSampling: stored.AllowSampling, TrustAnnotations: stored.TrustAnnotations, Version: stored.Version}
		return nil
	})
	return out, err
}

// CredentialInput is a credential body: a static value, or an OAuth client id and secret. Values are write-only.
type CredentialInput struct {
	Value        string
	ClientID     string
	ClientSecret string
}

// CredentialView is credential metadata; never a value.
type CredentialView struct {
	Scope     string
	Kind      string
	State     string
	Subject   string
	Scopes    []string
	ExpiresAt any
	RotatedAt any
	Record    *domain.McpCredentialRecord
}

func credentialView(scope string, kind domain.McpAuthKind, r *domain.McpCredentialRecord) CredentialView {
	v := CredentialView{Scope: scope, Kind: string(kind), State: domain.McpCredentialStateOf(kind, r), Scopes: []string{}, Record: r}
	if r != nil {
		v.Kind, v.Subject = r.Kind, r.Subject
		if r.Scopes != nil {
			v.Scopes = r.Scopes
		}
	}
	return v
}

// secretOf turns a credential body into the Secrets Manager value for the server's auth kind.
func secretOf(kind domain.McpAuthKind, in CredentialInput) (domain.SecretValue, error) {
	switch kind {
	case domain.McpAuthStatic:
		if in.ClientID != "" || in.ClientSecret != "" {
			return domain.SecretValue{}, domain.Validation(domain.FieldError{Field: "value", Reason: "a static credential takes value only"})
		}
		return domain.NewSecretValue("value", in.Value)
	case domain.McpAuthOAuthClient:
		if in.Value != "" || in.ClientID == "" || in.ClientSecret == "" || len(in.ClientID) > 500 {
			return domain.SecretValue{}, domain.Validation(domain.FieldError{Field: "clientSecret", Reason: "an OAuth client credential takes clientId and clientSecret"})
		}
		b, _ := json.Marshal(map[string]string{"clientId": in.ClientID, "clientSecret": in.ClientSecret})
		return domain.NewSecretValue("clientSecret", string(b))
	}
	return domain.SecretValue{}, domain.Validation(domain.FieldError{Field: "authKind", Reason: domain.ReasonAuthKindUnsupported})
}

// putCredential writes the value to Secrets Manager (a new secret at afs.mcp_credential_path, or a new version) and
// stores the ARN with state set. A secret created here is deleted again when the transaction fails.
func (s *Service) putCredential(ctx context.Context, c acting.Caller, action, serverID, scope string, in CredentialInput,
	expect *domain.Version, check func(srv domain.McpServerRecord) (teamID string, err error)) (CredentialView, error) {
	var out CredentialView
	var created string
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		srv, err := s.Repo.GetServer(ctx, tx, serverID, true)
		if err != nil {
			return err
		}
		teamID, err := check(srv)
		if err != nil {
			return err
		}
		val, err := secretOf(srv.AuthKind, in)
		if err != nil {
			return err
		}
		cur, err := s.Repo.GetCredential(ctx, tx, serverID, scope)
		if err != nil {
			return err
		}
		if expect != nil && cur != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		token := c.RequestID + "/" + serverID + "/" + scope
		now := s.Now()
		rec := domain.McpCredentialRecord{ServerID: serverID, Scope: scope, TeamID: teamID, Kind: string(srv.AuthKind),
			Status: domain.McpCredSet, RotatedAt: &now}
		if cur != nil {
			rec.ID, rec.SecretARN, rec.Version = cur.ID, cur.SecretARN, cur.Version
			if err := s.Secrets.PutValue(ctx, cur.SecretARN, val, token); err != nil {
				return err
			}
		} else {
			path, err := s.Repo.CredentialPath(ctx, tx, teamID, srv.Slug)
			if err != nil {
				return err
			}
			owner := teamID
			if owner == "" {
				owner = "_service"
			}
			arn, err := s.Secrets.Create(ctx, path, val, token, map[string]string{"afs-managed-by": "mcp-gateway",
				"afs-mcp-server-id": serverID, "afs-team-id": owner})
			if err != nil {
				return err
			}
			created = arn
			if rec.ID, err = s.newID(domain.PrefixMcpCredential); err != nil {
				return err
			}
			rec.SecretARN = arn
		}
		if err := s.Repo.UpsertCredential(ctx, tx, rec); err != nil {
			return err
		}
		stored, err := s.Repo.GetCredential(ctx, tx, serverID, scope)
		if err != nil {
			return err
		}
		meta := map[string]any{"scope": scope, "kind": rec.Kind, "state": domain.McpCredSet}
		var before any
		if cur != nil {
			before = map[string]any{"scope": scope, "kind": cur.Kind, "state": cur.Status}
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindCredential, rec.ID, teamID, before, meta)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.McpCredentialEvent(true), teamID, kindCredential, rec.ID, map[string]any{
			"serverId": serverID, "scope": scope, "kind": rec.Kind, "state": domain.McpCredSet}); err != nil {
			return err
		}
		out = credentialView(scope, srv.AuthKind, stored)
		return nil
	})
	if err != nil && created != "" {
		_ = s.Secrets.ForceDelete(context.WithoutCancel(ctx), created)
	}
	return out, err
}

// PutCredential implements PutMcpServerCredential: scope _shared (credentialMode shared) or a team id (per_team).
func (s *Service) PutCredential(ctx context.Context, c acting.Caller, serverID, scope string, in CredentialInput, expect *domain.Version) (CredentialView, error) {
	if err := s.checkService(ctx, c, "PutMcpServerCredential", authz.McpServersWrite); err != nil {
		return CredentialView{}, err
	}
	return s.putCredential(ctx, c, "PutMcpServerCredential", serverID, scope, in, expect, func(srv domain.McpServerRecord) (string, error) {
		switch {
		case srv.IsTeam() && scope == srv.TeamID:
			return srv.TeamID, nil
		case !srv.IsTeam() && srv.CredentialMode == domain.McpCredentialShared && scope == domain.McpSharedScope:
			return "", nil
		case !srv.IsTeam() && srv.CredentialMode == domain.McpCredentialPerTeam && scope != domain.McpSharedScope && domain.ID(scope).Prefix() == domain.PrefixTeam:
			return scope, nil
		}
		return "", domain.Validation(domain.FieldError{Field: "scope", Reason: "must be _shared for a shared credential, else the team id"})
	})
}
