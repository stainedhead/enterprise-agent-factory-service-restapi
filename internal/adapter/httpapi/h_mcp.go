package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// McpAPI serves the governed MCP server operations of API contract v1.6.0 §4.14 (release 1.3.0): the enterprise
// catalog and its review, team servers and switches, policy, credentials (write-only), the call ledger and usage
// reports, query capture, federated identities and the OAuth connect flow (h_mcp_oauth.go).
type McpAPI struct {
	Mcp   *mcpcatalog.Service
	Pages PageTokens
}

// BindMcp binds the MCP handlers.
func BindMcp(a *McpAPI) Registrar { return a.Register }

// Register binds the MCP operations.
func (a *McpAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListMcpServers": a.list, "CreateMcpServer": a.create, "GetMcpServer": a.get, "UpdateMcpServer": a.update,
		"DeleteMcpServer": a.delete, "ImportMcpServerManifest": a.importManifest,
		"PublishMcpServer": a.publish, "RetireMcpServer": a.retire, "SuspendMcpServer": a.suspend, "ResumeMcpServer": a.resume,
		"RefreshMcpInventory": a.refresh, "ListMcpPrimitives": a.listPrimitives, "GetMcpPrimitiveChange": a.primitiveChange,
		"ApproveMcpPrimitive": a.decidePrimitive(true), "RejectMcpPrimitive": a.decidePrimitive(false),
		"PutMcpPolicy": a.putPolicy, "PutMcpServerCredential": a.putCredential,
		"ApproveMcpServer": a.review(domain.McpApproved), "RejectMcpServer": a.review(domain.McpRejected),
		"RequestMcpServerChanges": a.review(domain.McpDraft), "ListMcpReviews": a.listReviews,
		"ListMcpEvents": a.listEvents, "GetMcpUsageReport": a.usage,
		"StartMcpQueryCapture": a.startCapture, "StopMcpQueryCapture": a.stopCapture, "GetMcpQueryCapture": a.getCapture,
		"ListTeamMcpServers": a.listTeam, "GetTeamMcpServer": a.getTeam, "PutTeamMcpServer": a.putSwitch,
		"CreateTeamMcpServer": a.createTeam, "UpdateTeamMcpServer": a.updateTeam, "DeleteTeamMcpServer": a.deleteTeam,
		"SubmitMcpServerForReview": a.submit, "ApproveTeamMcpServer": a.teamReview(true), "RejectTeamMcpServer": a.teamReview(false),
		"RefreshTeamMcpInventory": a.refreshTeam, "ListTeamMcpPrimitives": a.listTeamPrimitives,
		"ApproveTeamMcpPrimitive": a.teamDecidePrimitive(true), "RejectTeamMcpPrimitive": a.teamDecidePrimitive(false),
		"PutTeamMcpPolicy": a.putTeamPolicy, "PutTeamMcpServerCredential": a.putTeamCredential,
		"StartMcpOAuthConnect": a.startOAuth, "CompleteMcpOAuthConnect": a.completeOAuth, "DisconnectMcpOAuth": a.disconnectOAuth, "TestMcpFederation": a.testFederation,
		"GetEffectiveAgentMcp": a.effective("agent", "agentId"), "GetEffectivePoolMcp": a.effective("pool", "poolId"),
		"ListTeamMcpEvents": a.listTeamEvents, "GetTeamMcpUsageReport": a.teamUsage,
		"ListAgentFederatedIdentities": a.listFederated("agent", "agentId"), "PutAgentFederatedIdentity": a.putFederated("agent", "agentId"),
		"DeleteAgentFederatedIdentity": a.deleteFederated("agent", "agentId"),
		"ListPoolFederatedIdentities":  a.listFederated("pool", "poolId"), "PutPoolFederatedIdentity": a.putFederated("pool", "poolId"),
		"DeletePoolFederatedIdentity": a.deleteFederated("pool", "poolId"),
	})
}

// ---- request bodies ----

// mcpServerFields are the members every server body shares.
type mcpServerFields struct {
	DisplayName        *string               `json:"displayName"`
	Owner              *string               `json:"owner"`
	Purpose            *string               `json:"purpose"`
	DocURL             *string               `json:"docUrl"`
	DataClassification *string               `json:"dataClassification"`
	Transport          *string               `json:"transport"`
	Location           *string               `json:"location"`
	URL                *string               `json:"url"`
	CABundle           *string               `json:"caBundle"`
	Hosted             *domain.McpHosted     `json:"hosted"`
	AuthKind           *string               `json:"authKind"`
	AuthHeader         *string               `json:"authHeader"`
	Federation         *domain.McpFederation `json:"federation"`
	Limits             *domain.McpLimits     `json:"limits"`
}

// mcpServicePolicyFields are the enterprise-only members of create and update.
type mcpServicePolicyFields struct {
	CredentialMode   *string `json:"credentialMode"`
	DriftPolicy      *string `json:"driftPolicy"`
	AllowSampling    *bool   `json:"allowSampling"`
	TrustAnnotations *bool   `json:"trustAnnotations"`
}

func (f mcpServerFields) input() mcpcatalog.ServerInput {
	return mcpcatalog.ServerInput{DisplayName: f.DisplayName, Owner: f.Owner, Purpose: f.Purpose, DocURL: f.DocURL,
		DataClassification: f.DataClassification, Transport: f.Transport, Location: f.Location, URL: f.URL, CABundle: f.CABundle,
		Hosted: f.Hosted, AuthKind: f.AuthKind, AuthHeader: f.AuthHeader, Federation: f.Federation, Limits: f.Limits}
}

func (p mcpServicePolicyFields) apply(in *mcpcatalog.ServerInput) {
	in.CredentialMode, in.DriftPolicy, in.AllowSampling, in.TrustAnnotations = p.CredentialMode, p.DriftPolicy, p.AllowSampling, p.TrustAnnotations
}

// decodeOptional decodes a body that may be empty.
func decodeOptional(r *http.Request, raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return Decode(r, dst)
}

// mcpReviewBody is McpServerReviewRequest.
type mcpReviewBody struct {
	Note       string `json:"note"`
	Primitives []struct {
		PrimitiveID string `json:"primitiveId"`
		Decision    string `json:"decision"`
	} `json:"primitives"`
}

func (b mcpReviewBody) input() mcpcatalog.ReviewInput {
	in := mcpcatalog.ReviewInput{Note: b.Note}
	for _, p := range b.Primitives {
		in.Primitives = append(in.Primitives, mcpcatalog.PrimitiveDecision{PrimitiveID: p.PrimitiveID, Decision: p.Decision})
	}
	return in
}

type mcpCredentialRequest struct {
	Value        string `json:"value"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

func (b mcpCredentialRequest) input() mcpcatalog.CredentialInput {
	return mcpcatalog.CredentialInput{Value: b.Value, ClientID: b.ClientID, ClientSecret: b.ClientSecret}
}

// ---- shared shapes ----

// mcpIdem reads the body and builds the idempotency claim of op.
func mcpIdem(r *http.Request, op, teamID string, status int, body func(any) any) (acting.Caller, []byte, *acting.Idem, error) {
	c, err := w2Caller(r)
	if err != nil {
		return c, nil, nil, err
	}
	raw, err := w2Body(r)
	if err != nil {
		return c, nil, nil, err
	}
	idem, err := w2Idem(r, op, teamID, raw, func(v any) (int, json.RawMessage, error) { return w2Render(status, body(v)) })
	return c, raw, idem, err
}

func serverBodyOf(v any) any { return toMcpServerBody(v.(mcpcatalog.ServerView)) }

func primitiveBodyOf(v any) any { return toMcpPrimitiveBody(v.(mcpcatalog.PrimitiveView)) }

// writeServer writes a server answer or its replay.
func writeServer(w http.ResponseWriter, r *http.Request, status int, v mcpcatalog.ServerView, replay *port.StoredResponse, err error) {
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if replay != nil {
		w2Replay(w, replay)
		return
	}
	w2Etag(w, v.Server.Version)
	WriteJSON(w, status, toMcpServerBody(v))
}

func writeCreated(w http.ResponseWriter, r *http.Request, location string, res mcpcatalog.Created, err error) {
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w.Header().Set("Location", location+res.View.Server.ID)
	writeServer(w, r, http.StatusCreated, res.View, nil, nil)
}

// serverAction runs a POST action on a server with an optional body decoded into dst.
func (a *McpAPI) serverAction(w http.ResponseWriter, r *http.Request, op, teamID string, dst any,
	run func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error)) {
	c, raw, idem, err := mcpIdem(r, op, teamID, http.StatusOK, serverBodyOf)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if dst != nil {
		if err := decodeOptional(r, raw, dst); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	v, replay, err := run(c, idem)
	writeServer(w, r, http.StatusOK, v, replay, err)
}

func primitiveFilter(r *http.Request) mcpcatalog.PrimitiveFilter {
	q := r.URL.Query()
	return mcpcatalog.PrimitiveFilter{Kind: q.Get("kind"), Status: q.Get("status")}
}

// eventFilter reads the ledger query; teamId is honoured on the service path only.
func eventFilter(r *http.Request, service bool) (domain.McpEventFilter, error) {
	q := r.URL.Query()
	f := domain.McpEventFilter{AgentID: q.Get("agentId"), PoolID: q.Get("poolId"), ServerID: q.Get("serverId"), Method: q.Get("method"),
		Name: q.Get("name"), Outcome: q.Get("outcome"), Reason: q.Get("reason")}
	if service {
		f.TeamID = q.Get("teamId")
	}
	var err error
	if v := q.Get("from"); v != "" {
		if f.From, err = w2ParseTime("from", v); err != nil {
			return f, err
		}
	}
	if v := q.Get("to"); v != "" {
		if f.To, err = w2ParseTime("to", v); err != nil {
			return f, err
		}
	}
	return f, nil
}

func usageQuery(r *http.Request) (domain.McpUsageQuery, error) {
	q := r.URL.Query()
	u := domain.McpUsageQuery{GroupBy: q.Get("groupBy"), View: q.Get("view")}
	var err error
	if v := q.Get("from"); v != "" {
		if u.From, err = w2ParseTime("from", v); err != nil {
			return u, err
		}
	}
	if v := q.Get("to"); v != "" {
		if u.To, err = w2ParseTime("to", v); err != nil {
			return u, err
		}
	}
	if v := q.Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 30 && n != 90) {
			return u, bad("window", "must be 30 or 90")
		}
		u.WindowDays = n
	}
	return u, nil
}

// ---- enterprise catalog ----

func (a *McpAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := mcpcatalog.ServerFilter{Scope: q.Get("scope"), ReviewStatus: q.Get("reviewStatus"), RiskTier: q.Get("riskTier"),
		TeamID: q.Get("teamId"), Location: q.Get("location")}
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]mcpcatalog.ServerView, string, error) {
		return a.Mcp.List(r.Context(), c, f, p)
	}, toMcpServerBody)
}

func (a *McpAPI) create(w http.ResponseWriter, r *http.Request) {
	c, _, idem, err := mcpIdem(r, "CreateMcpServer", "", http.StatusCreated, serverBodyOf)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Slug *string `json:"slug"`
		mcpServerFields
		mcpServicePolicyFields
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := req.input()
	in.Slug = req.Slug
	req.apply(&in)
	res, err := a.Mcp.Create(r.Context(), c, in, idem)
	writeCreated(w, r, "/v1/mcp-servers/", res, err)
}

func (a *McpAPI) importManifest(w http.ResponseWriter, r *http.Request) {
	c, _, idem, err := mcpIdem(r, "ImportMcpServerManifest", "", http.StatusCreated, serverBodyOf)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Manifest json.RawMessage `json:"manifest"`
		Slug     string          `json:"slug"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(req.Manifest) == 0 {
		WriteError(w, r, bad("manifest", "is required"))
		return
	}
	res, err := a.Mcp.Import(r.Context(), c, req.Manifest, req.Slug, idem)
	writeCreated(w, r, "/v1/mcp-servers/", res, err)
}

func (a *McpAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Mcp.Get(r.Context(), c, r.PathValue("mcpServerId"))
	writeServer(w, r, http.StatusOK, v, nil, err)
}

func (a *McpAPI) update(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		mcpServerFields
		mcpServicePolicyFields
		DefaultForNewTeams *bool `json:"defaultForNewTeams"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := req.input()
	req.apply(&in)
	in.DefaultForNewTeams = req.DefaultForNewTeams
	v, err := a.Mcp.Update(r.Context(), c, r.PathValue("mcpServerId"), in, expect)
	writeServer(w, r, http.StatusOK, v, nil, err)
}

func (a *McpAPI) delete(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Mcp.Delete(r.Context(), c, r.PathValue("mcpServerId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// noteBody is McpReviewNoteRequest; publish and retire record nothing of it beyond the audit trail's request.
type noteBody struct {
	Note string `json:"note"`
}

func (a *McpAPI) publish(w http.ResponseWriter, r *http.Request) {
	a.serverAction(w, r, "PublishMcpServer", "", &noteBody{}, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
		return a.Mcp.Publish(r.Context(), c, r.PathValue("mcpServerId"), idem)
	})
}

func (a *McpAPI) retire(w http.ResponseWriter, r *http.Request) {
	a.serverAction(w, r, "RetireMcpServer", "", &noteBody{}, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
		return a.Mcp.Retire(r.Context(), c, r.PathValue("mcpServerId"), idem)
	})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (a *McpAPI) suspend(w http.ResponseWriter, r *http.Request) {
	var b reasonBody
	a.serverAction(w, r, "SuspendMcpServer", "", &b, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
		return a.Mcp.Suspend(r.Context(), c, r.PathValue("mcpServerId"), b.Reason, idem)
	})
}

func (a *McpAPI) resume(w http.ResponseWriter, r *http.Request) {
	var b reasonBody
	a.serverAction(w, r, "ResumeMcpServer", "", &b, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
		return a.Mcp.Resume(r.Context(), c, r.PathValue("mcpServerId"), b.Reason, idem)
	})
}

// refreshed answers 202 with the server as it stands; the gateway refreshes the inventory on its next pass.
func (a *McpAPI) refreshed(w http.ResponseWriter, r *http.Request, run func(c acting.Caller) error, get func(c acting.Caller) (mcpcatalog.ServerView, error)) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := run(c); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := get(c)
	writeServer(w, r, http.StatusAccepted, v, nil, err)
}

func (a *McpAPI) refresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("mcpServerId")
	a.refreshed(w, r, func(c acting.Caller) error { return a.Mcp.Refresh(r.Context(), c, id) },
		func(c acting.Caller) (mcpcatalog.ServerView, error) { return a.Mcp.Get(r.Context(), c, id) })
}

func (a *McpAPI) listPrimitives(w http.ResponseWriter, r *http.Request) {
	f := primitiveFilter(r)
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]mcpcatalog.PrimitiveView, string, error) {
		return a.Mcp.ListPrimitives(r.Context(), c, r.PathValue("mcpServerId"), f, p)
	}, toMcpPrimitiveBody)
}

func (a *McpAPI) primitiveChange(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ch, err := a.Mcp.GetPrimitiveChange(r.Context(), c, r.PathValue("mcpServerId"), r.PathValue("primitiveId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toMcpPrimitiveChangeBody(ch))
}

// primitiveAction runs approve or reject of one primitive.
func primitiveAction(w http.ResponseWriter, r *http.Request, op, teamID string,
	run func(c acting.Caller, note string, idem *acting.Idem) (mcpcatalog.PrimitiveView, *port.StoredResponse, error)) {
	c, raw, idem, err := mcpIdem(r, op, teamID, http.StatusOK, primitiveBodyOf)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var b noteBody
	if err := decodeOptional(r, raw, &b); err != nil {
		WriteError(w, r, err)
		return
	}
	v, replay, err := run(c, b.Note, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if replay != nil {
		w2Replay(w, replay)
		return
	}
	WriteJSON(w, http.StatusOK, toMcpPrimitiveBody(v))
}

func (a *McpAPI) decidePrimitive(approve bool) http.HandlerFunc {
	op := "RejectMcpPrimitive"
	if approve {
		op = "ApproveMcpPrimitive"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		primitiveAction(w, r, op, "", func(c acting.Caller, note string, idem *acting.Idem) (mcpcatalog.PrimitiveView, *port.StoredResponse, error) {
			return a.Mcp.DecidePrimitive(r.Context(), c, r.PathValue("mcpServerId"), r.PathValue("primitiveId"), note, approve, idem)
		})
	}
}

func writePolicy(w http.ResponseWriter, r *http.Request, p mcpcatalog.PolicyView, err error) {
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusOK, toMcpPolicyBody(p))
}

func (a *McpAPI) putPolicy(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Rules            []domain.McpPolicyRule `json:"rules"`
		DriftPolicy      *string                `json:"driftPolicy"`
		AllowSampling    *bool                  `json:"allowSampling"`
		TrustAnnotations *bool                  `json:"trustAnnotations"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Rules == nil {
		WriteError(w, r, bad("rules", "is required"))
		return
	}
	p, err := a.Mcp.PutPolicy(r.Context(), c, r.PathValue("mcpServerId"), mcpcatalog.PolicyInput{Rules: req.Rules, DriftPolicy: req.DriftPolicy,
		AllowSampling: req.AllowSampling, TrustAnnotations: req.TrustAnnotations}, expect)
	writePolicy(w, r, p, err)
}

func writeCredential(w http.ResponseWriter, r *http.Request, v mcpcatalog.CredentialView, err error) {
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if v.Record != nil {
		w2Etag(w, v.Record.Version)
	}
	WriteJSON(w, http.StatusOK, toMcpCredentialBody(v))
}

// credentialRequest decodes a credential body; the value lives only in this request.
func credentialRequest(r *http.Request) (acting.Caller, *domain.Version, mcpcatalog.CredentialInput, error) {
	c, err := w2Caller(r)
	if err != nil {
		return c, nil, mcpcatalog.CredentialInput{}, err
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		return c, nil, mcpcatalog.CredentialInput{}, err
	}
	var req mcpCredentialRequest
	if err := Decode(r, &req); err != nil {
		return c, nil, mcpcatalog.CredentialInput{}, err
	}
	return c, expect, req.input(), nil
}

func (a *McpAPI) putCredential(w http.ResponseWriter, r *http.Request) {
	c, expect, in, err := credentialRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Mcp.PutCredential(r.Context(), c, r.PathValue("mcpServerId"), r.PathValue("scope"), in, expect)
	writeCredential(w, r, v, err)
}

func (a *McpAPI) review(to domain.McpReviewStatus) http.HandlerFunc {
	op := map[domain.McpReviewStatus]string{domain.McpApproved: "ApproveMcpServer", domain.McpRejected: "RejectMcpServer",
		domain.McpDraft: "RequestMcpServerChanges"}[to]
	return func(w http.ResponseWriter, r *http.Request) {
		var b mcpReviewBody
		a.serverAction(w, r, op, "", &b, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
			return a.Mcp.Review(r.Context(), c, r.PathValue("mcpServerId"), to, b.input(), idem)
		})
	}
}

func (a *McpAPI) listReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := mcpcatalog.ReviewQueueFilter{Kind: q.Get("kind"), RiskTier: q.Get("riskTier"), TeamID: q.Get("teamId"), Approver: q.Get("approver")}
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.McpReviewQueueItem, string, error) {
		return a.Mcp.ListReviews(r.Context(), c, f, p)
	}, toMcpReviewItemBody)
}

// ---- ledger, reports and capture ----

func (a *McpAPI) events(w http.ResponseWriter, r *http.Request, service bool) {
	f, err := eventFilter(r, service)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.McpEventRecord, string, error) {
		if service {
			return a.Mcp.ListEvents(r.Context(), c, f, p)
		}
		return a.Mcp.ListTeamEvents(r.Context(), c, r.PathValue("teamId"), f, p)
	}, toMcpEventBody)
}

func (a *McpAPI) listEvents(w http.ResponseWriter, r *http.Request)     { a.events(w, r, true) }
func (a *McpAPI) listTeamEvents(w http.ResponseWriter, r *http.Request) { a.events(w, r, false) }

func (a *McpAPI) report(w http.ResponseWriter, r *http.Request, team bool) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q, err := usageQuery(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var rows []domain.McpUsageRow
	if team {
		rows, q, err = a.Mcp.TeamUsage(r.Context(), c, r.PathValue("teamId"), q)
	} else {
		rows, q, err = a.Mcp.Usage(r.Context(), c, q)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toMcpUsageReportBody(rows, q))
}

func (a *McpAPI) usage(w http.ResponseWriter, r *http.Request)     { a.report(w, r, false) }
func (a *McpAPI) teamUsage(w http.ResponseWriter, r *http.Request) { a.report(w, r, true) }

func (a *McpAPI) startCapture(w http.ResponseWriter, r *http.Request) {
	c, _, idem, err := mcpIdem(r, "StartMcpQueryCapture", "", http.StatusCreated, func(v any) any { return toMcpCaptureBody(v.(domain.McpQueryCapture)) })
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Days   *int   `json:"days"`
		Reason string `json:"reason"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Days == nil {
		WriteError(w, r, bad("days", "is required"))
		return
	}
	res, err := a.Mcp.StartCapture(r.Context(), c, *req.Days, req.Reason, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusCreated, toMcpCaptureBody(res.Capture))
}

func (a *McpAPI) stopCapture(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Mcp.StopCapture(r.Context(), c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toMcpCaptureBody(x))
}

func (a *McpAPI) getCapture(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Mcp.GetCapture(r.Context(), c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var body struct {
		Capture *mcpCaptureBody `json:"capture"`
	}
	if x != nil {
		b := toMcpCaptureBody(*x)
		body.Capture = &b
	}
	WriteJSON(w, http.StatusOK, body)
}

// ---- team servers ----

func (a *McpAPI) listTeam(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("teamId")
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]mcpcatalog.TeamServerView, string, error) {
		return a.Mcp.ListTeam(r.Context(), c, teamID, r.URL.Query().Get("state"), p)
	}, func(v mcpcatalog.TeamServerView) teamMcpServerBody { return toTeamMcpServerBody(v, teamID) })
}

func writeTeamServer(w http.ResponseWriter, r *http.Request, status int, teamID string, v mcpcatalog.TeamServerView, err error) {
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if v.Row.Switch != nil {
		w2Etag(w, v.Row.Switch.Version)
	}
	WriteJSON(w, status, toTeamMcpServerBody(v, teamID))
}

func (a *McpAPI) getTeam(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	v, err := a.Mcp.GetTeam(r.Context(), c, teamID, r.PathValue("mcpServerId"))
	writeTeamServer(w, r, http.StatusOK, teamID, v, err)
}

func (a *McpAPI) putSwitch(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Enabled *bool   `json:"enabled"`
		Mode    *string `json:"mode"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Enabled == nil {
		WriteError(w, r, bad("enabled", "is required"))
		return
	}
	in := mcpcatalog.SwitchInput{Enabled: *req.Enabled}
	if req.Mode != nil {
		in.Mode = domain.McpTeamMode(*req.Mode)
	}
	teamID := r.PathValue("teamId")
	v, err := a.Mcp.PutSwitch(r.Context(), c, teamID, r.PathValue("mcpServerId"), in, expect)
	writeTeamServer(w, r, http.StatusOK, teamID, v, err)
}

func (a *McpAPI) createTeam(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("teamId")
	c, _, idem, err := mcpIdem(r, "CreateTeamMcpServer", teamID, http.StatusCreated, serverBodyOf)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Slug *string `json:"slug"`
		mcpServerFields
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := req.input()
	in.Slug = req.Slug
	res, err := a.Mcp.CreateTeam(r.Context(), c, teamID, in, idem)
	writeCreated(w, r, "/v1/teams/"+teamID+"/mcp-servers/", res, err)
}

func (a *McpAPI) updateTeam(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req mcpServerFields
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Mcp.UpdateTeam(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"), req.input(), expect)
	writeServer(w, r, http.StatusOK, v, nil, err)
}

func (a *McpAPI) deleteTeam(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Mcp.DeleteTeam(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *McpAPI) submit(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("teamId")
	a.serverAction(w, r, "SubmitMcpServerForReview", teamID, nil, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
		return a.Mcp.Submit(r.Context(), c, teamID, r.PathValue("mcpServerId"), idem)
	})
}

func (a *McpAPI) teamReview(approve bool) http.HandlerFunc {
	op := "RejectTeamMcpServer"
	if approve {
		op = "ApproveTeamMcpServer"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		teamID := r.PathValue("teamId")
		var b mcpReviewBody
		a.serverAction(w, r, op, teamID, &b, func(c acting.Caller, idem *acting.Idem) (mcpcatalog.ServerView, *port.StoredResponse, error) {
			return a.Mcp.TeamReview(r.Context(), c, teamID, r.PathValue("mcpServerId"), approve, b.input(), idem)
		})
	}
}

func (a *McpAPI) refreshTeam(w http.ResponseWriter, r *http.Request) {
	teamID, id := r.PathValue("teamId"), r.PathValue("mcpServerId")
	a.refreshed(w, r, func(c acting.Caller) error { return a.Mcp.RefreshTeam(r.Context(), c, teamID, id) },
		func(c acting.Caller) (mcpcatalog.ServerView, error) {
			v, err := a.Mcp.GetTeam(r.Context(), c, teamID, id)
			return mcpcatalog.ServerView{Server: v.Row.Server, CredentialState: v.CredentialState, Approver: v.Approver}, err
		})
}

func (a *McpAPI) listTeamPrimitives(w http.ResponseWriter, r *http.Request) {
	f := primitiveFilter(r)
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]mcpcatalog.PrimitiveView, string, error) {
		return a.Mcp.ListTeamPrimitives(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"), f, p)
	}, toMcpPrimitiveBody)
}

func (a *McpAPI) teamDecidePrimitive(approve bool) http.HandlerFunc {
	op := "RejectTeamMcpPrimitive"
	if approve {
		op = "ApproveTeamMcpPrimitive"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		teamID := r.PathValue("teamId")
		primitiveAction(w, r, op, teamID, func(c acting.Caller, note string, idem *acting.Idem) (mcpcatalog.PrimitiveView, *port.StoredResponse, error) {
			return a.Mcp.TeamDecidePrimitive(r.Context(), c, teamID, r.PathValue("mcpServerId"), r.PathValue("primitiveId"), note, approve, idem)
		})
	}
}

func (a *McpAPI) putTeamPolicy(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Rules []domain.McpPolicyRule `json:"rules"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Rules == nil {
		WriteError(w, r, bad("rules", "is required"))
		return
	}
	p, err := a.Mcp.PutTeamPolicy(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"), req.Rules, expect)
	writePolicy(w, r, p, err)
}

func (a *McpAPI) putTeamCredential(w http.ResponseWriter, r *http.Request) {
	c, expect, in, err := credentialRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Mcp.PutTeamCredential(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"), in, expect)
	writeCredential(w, r, v, err)
}

func (a *McpAPI) disconnectOAuth(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("teamId")
	c, _, idem, err := mcpIdem(r, "DisconnectMcpOAuth", teamID, http.StatusOK, func(v any) any { return toMcpCredentialBody(v.(mcpcatalog.CredentialView)) })
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, replay, err := a.Mcp.DisconnectOAuth(r.Context(), c, teamID, r.PathValue("mcpServerId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if replay != nil {
		w2Replay(w, replay)
		return
	}
	WriteJSON(w, http.StatusOK, toMcpCredentialBody(v))
}

func (a *McpAPI) testFederation(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	v, err := a.Mcp.TestFederation(r.Context(), c, teamID, r.PathValue("mcpServerId"))
	writeTeamServer(w, r, http.StatusAccepted, teamID, v, err)
}

func (a *McpAPI) effective(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := w2Caller(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		e, err := a.Mcp.GetEffective(r.Context(), c, r.PathValue("teamId"), kind, r.PathValue(param))
		if err != nil {
			WriteError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, toEffectiveMcpBody(e))
	}
}

// ---- federated identities ----

func (a *McpAPI) listFederated(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := w2Caller(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		rows, err := a.Mcp.ListFederated(r.Context(), c, r.PathValue("teamId"), kind, r.PathValue(param))
		if err != nil {
			WriteError(w, r, err)
			return
		}
		out := w2ListBody[federatedIdentityBody]{Items: make([]federatedIdentityBody, 0, len(rows))}
		for _, f := range rows {
			out.Items = append(out.Items, toFederatedIdentityBody(f))
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

func (a *McpAPI) putFederated(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := w2Caller(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		expect, err := w2IfMatchOptional(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var req struct {
			Audience          string `json:"audience"`
			ExpirationSeconds int    `json:"expirationSeconds"`
			FilePath          string `json:"filePath"`
			EnvVar            string `json:"envVar"`
		}
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		v, err := a.Mcp.PutFederated(r.Context(), c, r.PathValue("teamId"), kind, r.PathValue(param), domain.FederatedIdentity{Name: r.PathValue("name"),
			Audience: req.Audience, ExpirationSeconds: req.ExpirationSeconds, FilePath: req.FilePath, EnvVar: req.EnvVar}, expect)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		w2Etag(w, v.Identity.Version)
		WriteJSON(w, http.StatusOK, toFederatedIdentityBody(v))
	}
}

func (a *McpAPI) deleteFederated(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := w2Caller(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		expect, err := w2IfMatch(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if err := a.Mcp.DeleteFederated(r.Context(), c, r.PathValue("teamId"), kind, r.PathValue(param), r.PathValue("name"), expect); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
