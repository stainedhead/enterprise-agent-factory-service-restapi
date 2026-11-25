package httpapi

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	me "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
)

// ModelEndpointsAPI serves the 27 operations of API contract v1.5.0 §4.13: service model endpoints,
// their models, prices, proposals, credentials, federations and statements; the provider switches; a
// team's gateway settings, route selections and own endpoints; and the inference reports.
type ModelEndpointsAPI struct {
	Svc   *me.Service
	Pages PageTokens
}

// BindModelEndpoints binds the model endpoint handlers.
func BindModelEndpoints(a *ModelEndpointsAPI) Registrar { return a.Register }

// Register binds the §4.13 operations.
func (a *ModelEndpointsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListModelEndpoints":             a.list,
		"CreateModelEndpoint":            a.create,
		"GetModelEndpoint":               a.get,
		"UpdateModelEndpoint":            a.update,
		"DeleteModelEndpoint":            a.delete,
		"PutEndpointModels":              a.putModels,
		"CreateEndpointModelPrice":       a.createPrice,
		"ListEndpointPriceProposals":     a.listProposals,
		"ApproveEndpointPriceProposal":   a.approve,
		"RejectEndpointPriceProposal":    a.reject,
		"ListEndpointTeams":              a.listTeams,
		"PutModelEndpointCredential":     a.putCredential,
		"RotateModelEndpointCredential":  a.rotateCredential,
		"PutEndpointFederation":          a.putFederation,
		"ImportProviderStatement":        a.importStatement,
		"ListInferenceProviders":         a.listProviders,
		"UpdateInferenceProvider":        a.updateProvider,
		"PutTeamGatewaySettings":         a.putGateway,
		"ListTeamModelEndpoints":         a.listTeamEndpoints,
		"PutTeamModelEndpoints":          a.putSelections,
		"CreateTeamModelEndpoint":        a.createTeamEndpoint,
		"UpdateTeamModelEndpoint":        a.updateTeamEndpoint,
		"DeleteTeamModelEndpoint":        a.deleteTeamEndpoint,
		"PutTeamEndpointModels":          a.putTeamModels,
		"PutTeamModelEndpointCredential": a.putTeamCredential,
		"GetInferenceReport":             a.report,
		"GetTeamInferenceReport":         a.report,
	})
}

// ---- request shapes ----

type priceInput struct {
	InputUsdPerMtok       domain.Money  `json:"inputUsdPerMtok"`
	OutputUsdPerMtok      domain.Money  `json:"outputUsdPerMtok"`
	CachedInputUsdPerMtok *domain.Money `json:"cachedInputUsdPerMtok"`
	RequestFeeUsd         *domain.Money `json:"requestFeeUsd"`
}

func (p *priceInput) price() *me.Price {
	if p == nil {
		return nil
	}
	out := &me.Price{Input: p.InputUsdPerMtok, Output: p.OutputUsdPerMtok, CachedInput: p.CachedInputUsdPerMtok}
	if p.RequestFeeUsd != nil {
		out.Fee = *p.RequestFeeUsd
	}
	return out
}

type modelInput struct {
	ModelName       string      `json:"modelName"`
	MaxOutputTokens int         `json:"maxOutputTokens"`
	ContextTokens   *int        `json:"contextTokens"`
	Enabled         *bool       `json:"enabled"`
	Price           *priceInput `json:"price"`
}

func modelInputs(in []modelInput, priced bool) ([]me.ModelInput, error) {
	out := make([]me.ModelInput, 0, len(in))
	for i, m := range in {
		if m.Price != nil && !priced {
			return nil, bad("models["+strconv.Itoa(i)+"].price", "unknown field")
		}
		mi := me.ModelInput{EndpointModel: domain.EndpointModel{ModelName: m.ModelName, MaxOutputTokens: m.MaxOutputTokens, Enabled: m.Enabled == nil || *m.Enabled},
			Price: m.Price.price()}
		if m.ContextTokens != nil {
			mi.ContextTokens = *m.ContextTokens
		}
		out = append(out, mi)
	}
	return out, nil
}

func protocolsOf(in []string) []domain.Protocol {
	out := make([]domain.Protocol, 0, len(in))
	for _, p := range in {
		out = append(out, domain.Protocol(p))
	}
	return out
}

type createEndpointRequest struct {
	Slug             string       `json:"slug"`
	DisplayName      string       `json:"displayName"`
	Description      string       `json:"description"`
	Owner            string       `json:"owner"`
	Purpose          string       `json:"purpose"`
	Provider         string       `json:"provider"`
	Protocols        []string     `json:"protocols"`
	Location         string       `json:"location"`
	BaseURL          string       `json:"baseUrl"`
	CABundle         *string      `json:"caBundle"`
	AuthKind         string       `json:"authKind"`
	AuthHeader       *string      `json:"authHeader"`
	MetricsPort      *int         `json:"metricsPort"`
	CredentialMode   string       `json:"credentialMode"`
	PricingMode      string       `json:"pricingMode"`
	RequiresTerms    bool         `json:"requiresTerms"`
	UseProviderCost  bool         `json:"useProviderCost"`
	TimeoutSeconds   int          `json:"timeoutSeconds"`
	MaxBodyMib       int          `json:"maxBodyMib"`
	ExternalSystemID string       `json:"externalSystemId"`
	Models           []modelInput `json:"models"`
}

func (q createEndpointRequest) endpoint() domain.ModelEndpoint {
	e := domain.ModelEndpoint{Slug: q.Slug, DisplayName: q.DisplayName, Description: q.Description, Owner: q.Owner, Purpose: q.Purpose,
		Provider: q.Provider, Protocols: protocolsOf(q.Protocols), Location: domain.EndpointLocation(q.Location), BaseURL: q.BaseURL,
		AuthKind: domain.AuthKind(q.AuthKind), CredentialMode: domain.CredentialMode(q.CredentialMode), PricingMode: domain.PricingMode(q.PricingMode),
		RequiresTerms: q.RequiresTerms, UseProviderCost: q.UseProviderCost, TimeoutSeconds: q.TimeoutSeconds, MaxBodyMiB: q.MaxBodyMib,
		ExternalSystem: q.ExternalSystemID}
	if q.CABundle != nil {
		e.CABundle = *q.CABundle
	}
	if q.AuthHeader != nil {
		e.AuthHeader = *q.AuthHeader
	}
	if q.MetricsPort != nil {
		e.MetricsPort = *q.MetricsPort
	}
	if e.CredentialMode == "" {
		e.CredentialMode = domain.CredentialPerTeam
	}
	return e
}

type updateEndpointRequest struct {
	DisplayName      *string            `json:"displayName"`
	Description      *string            `json:"description"`
	Owner            *string            `json:"owner"`
	Purpose          *string            `json:"purpose"`
	Provider         *string            `json:"provider"`
	Protocols        *[]string          `json:"protocols"`
	Location         *string            `json:"location"`
	BaseURL          *string            `json:"baseUrl"`
	CABundle         w2Nullable[string] `json:"caBundle"`
	AuthKind         *string            `json:"authKind"`
	AuthHeader       w2Nullable[string] `json:"authHeader"`
	MetricsPort      w2Nullable[int]    `json:"metricsPort"`
	CredentialMode   *string            `json:"credentialMode"`
	PricingMode      *string            `json:"pricingMode"`
	RequiresTerms    *bool              `json:"requiresTerms"`
	UseProviderCost  *bool              `json:"useProviderCost"`
	Enabled          *bool              `json:"enabled"`
	TimeoutSeconds   *int               `json:"timeoutSeconds"`
	MaxBodyMib       *int               `json:"maxBodyMib"`
	ExternalSystemID w2Nullable[string] `json:"externalSystemId"`
}

func nullable[T any](n w2Nullable[T]) *T {
	if !n.Set {
		return nil
	}
	v := n.Value
	if n.Null {
		var zero T
		v = zero
	}
	return &v
}

func (q updateEndpointRequest) patch() me.Patch {
	p := me.Patch{DisplayName: q.DisplayName, Description: q.Description, Owner: q.Owner, Purpose: q.Purpose, Provider: q.Provider,
		BaseURL: q.BaseURL, CABundle: nullable(q.CABundle), AuthHeader: nullable(q.AuthHeader), MetricsPort: nullable(q.MetricsPort),
		ExternalSystem: nullable(q.ExternalSystemID), RequiresTerms: q.RequiresTerms, UseProviderCost: q.UseProviderCost, Enabled: q.Enabled,
		TimeoutSeconds: q.TimeoutSeconds, MaxBodyMiB: q.MaxBodyMib}
	if q.Protocols != nil {
		ps := protocolsOf(*q.Protocols)
		p.Protocols = &ps
	}
	if q.Location != nil {
		l := domain.EndpointLocation(*q.Location)
		p.Location = &l
	}
	if q.AuthKind != nil {
		k := domain.AuthKind(*q.AuthKind)
		p.AuthKind = &k
	}
	if q.CredentialMode != nil {
		m := domain.CredentialMode(*q.CredentialMode)
		p.CredentialMode = &m
	}
	if q.PricingMode != nil {
		m := domain.PricingMode(*q.PricingMode)
		p.PricingMode = &m
	}
	return p
}

// teamFieldsOnly refuses the members a team's own endpoint does not take (API contract §4.13).
func (q updateEndpointRequest) teamFieldsOnly() error {
	switch {
	case q.Provider != nil:
		return bad("provider", "unknown field")
	case q.CredentialMode != nil:
		return bad("credentialMode", "unknown field")
	case q.RequiresTerms != nil:
		return bad("requiresTerms", "unknown field")
	case q.UseProviderCost != nil:
		return bad("useProviderCost", "unknown field")
	case q.ExternalSystemID.Set:
		return bad("externalSystemId", "unknown field")
	}
	return nil
}

// serviceFieldsOnly refuses the members only a team's own endpoint takes.
func (q updateEndpointRequest) serviceFieldsOnly() error {
	if q.MetricsPort.Set {
		return bad("metricsPort", "unknown field")
	}
	return nil
}

// ---- service endpoints ----

func (a *ModelEndpointsAPI) list(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	f := me.Filter{IncludeTeams: qs.Get("includeTeams") == "true", Provider: qs.Get("provider"), Location: qs.Get("location"), PricingMode: qs.Get("pricingMode")}
	if v := qs.Get("includeTeams"); v != "" && v != "true" && v != "false" {
		WriteError(w, r, bad("includeTeams", "must be true or false"))
		return
	}
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]me.Endpoint, string, error) {
		return a.Svc.ListEndpoints(r.Context(), c, f, p)
	}, toModelEndpointBody)
}

func (a *ModelEndpointsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.Svc.GetEndpoint(r.Context(), c, r.PathValue("endpointId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, e.Version())
	WriteJSON(w, http.StatusOK, toModelEndpointBody(e))
}

// created runs a create with its idempotency claim and writes 201 with the body and ETag.
func created[T any, B any](w http.ResponseWriter, r *http.Request, op, teamID string, raw []byte, body func(T) B, version func(T) domain.Version,
	do func(c acting.Caller, idem *acting.Idem) (me.Created[T], error)) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, op, teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, body(v.(T)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := do(c, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	if version != nil {
		w2Etag(w, version(res.Value))
	}
	WriteJSON(w, http.StatusCreated, body(res.Value))
}

func endpointVersion(e me.Endpoint) domain.Version { return e.Version() }

func (a *ModelEndpointsAPI) create(w http.ResponseWriter, r *http.Request) {
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createEndpointRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Location == string(domain.LocationInNamespace) {
		WriteError(w, r, bad("location", "must be external or corporate"))
		return
	}
	if req.MetricsPort != nil {
		WriteError(w, r, bad("metricsPort", "unknown field"))
		return
	}
	models, err := modelInputs(req.Models, false)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	in := me.NewEndpoint{Endpoint: req.endpoint(), Models: models}
	created(w, r, "CreateModelEndpoint", "", raw, toModelEndpointBody, endpointVersion, func(c acting.Caller, idem *acting.Idem) (me.Created[me.Endpoint], error) {
		return a.Svc.CreateEndpoint(r.Context(), c, in, idem)
	})
}

func (a *ModelEndpointsAPI) update(w http.ResponseWriter, r *http.Request) {
	a.patchEndpoint(w, r, "", func(c acting.Caller, p me.Patch, v domain.Version) (me.Endpoint, error) {
		return a.Svc.UpdateEndpoint(r.Context(), c, r.PathValue("endpointId"), p, v)
	})
}

func (a *ModelEndpointsAPI) patchEndpoint(w http.ResponseWriter, r *http.Request, teamID string, do func(acting.Caller, me.Patch, domain.Version) (me.Endpoint, error)) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateEndpointRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	check := req.serviceFieldsOnly
	if teamID != "" {
		check = req.teamFieldsOnly
	} else if req.Location != nil && *req.Location == string(domain.LocationInNamespace) {
		WriteError(w, r, bad("location", "must be external or corporate"))
		return
	}
	if err := check(); err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := do(c, req.patch(), v)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, e.Version())
	WriteJSON(w, http.StatusOK, toModelEndpointBody(e))
}

func (a *ModelEndpointsAPI) delete(w http.ResponseWriter, r *http.Request) {
	a.deleteWith(w, r, func(c acting.Caller, v domain.Version) error {
		return a.Svc.DeleteEndpoint(r.Context(), c, r.PathValue("endpointId"), v)
	})
}

func (a *ModelEndpointsAPI) deleteWith(w http.ResponseWriter, r *http.Request, do func(acting.Caller, domain.Version) error) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := do(c, v); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *ModelEndpointsAPI) putModels(w http.ResponseWriter, r *http.Request) {
	a.putModelsWith(w, r, false, func(c acting.Caller, ms []me.ModelInput, v *domain.Version) (me.ModelList, domain.PricingMode, error) {
		l, err := a.Svc.PutModels(r.Context(), c, r.PathValue("endpointId"), ms, v)
		return l, modeOf(l), err
	})
}

// modeOf returns the pricing mode of the endpoint of a model list, for the callable flags.
func modeOf(l me.ModelList) domain.PricingMode {
	for _, m := range l.Models {
		if m.PriceState == me.PriceNotRequired {
			return domain.PricingFree
		}
	}
	return domain.PricingPerToken
}

func (a *ModelEndpointsAPI) putModelsWith(w http.ResponseWriter, r *http.Request, priced bool,
	do func(acting.Caller, []me.ModelInput, *domain.Version) (me.ModelList, domain.PricingMode, error)) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Models *[]modelInput `json:"models"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Models == nil {
		WriteError(w, r, bad("models", "is required"))
		return
	}
	ms, err := modelInputs(*req.Models, priced)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, mode, err := do(c, ms, v)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, l.Version)
	WriteJSON(w, http.StatusOK, toEndpointModelList(l, mode))
}

func (a *ModelEndpointsAPI) createPrice(w http.ResponseWriter, r *http.Request) {
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		ModelName     string `json:"modelName"`
		EffectiveFrom string `json:"effectiveFrom"`
		Source        string `json:"source"`
		priceInput
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	eff, err := w2ParseTime("effectiveFrom", req.EffectiveFrom)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	source := req.Source
	if source == "" {
		source = "manual"
	}
	id := r.PathValue("endpointId")
	row := me.PriceRow{ModelName: req.ModelName, EffectiveFrom: eff, Price: *req.price(), Source: source}
	created(w, r, "CreateEndpointModelPrice", "", append([]byte(id+"\n"), raw...), toEndpointPriceBody, nil,
		func(c acting.Caller, idem *acting.Idem) (me.Created[me.PriceRow], error) {
			return a.Svc.CreatePrice(r.Context(), c, id, row, idem)
		})
}

func (a *ModelEndpointsAPI) listProposals(w http.ResponseWriter, r *http.Request) {
	state, endpointID := r.URL.Query().Get("status"), r.URL.Query().Get("endpointId")
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]me.Proposal, string, error) {
		return a.Svc.ListProposals(r.Context(), c, state, endpointID, p)
	}, toEndpointProposalBody)
}

func (a *ModelEndpointsAPI) approve(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "ApproveEndpointPriceProposal", a.Svc.Approve)
}

func (a *ModelEndpointsAPI) reject(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "RejectEndpointPriceProposal", a.Svc.Reject)
}

func (a *ModelEndpointsAPI) decide(w http.ResponseWriter, r *http.Request, op string,
	do func(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (me.Decided, error)) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Note *string `json:"note"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	id := r.PathValue("proposalId")
	idem, err := w2Idem(r, op, "", append([]byte(id+"\n"), raw...), func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toEndpointProposalBody(v.(me.Proposal)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := do(r.Context(), c, id, note, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Value.Version())
	WriteJSON(w, http.StatusOK, toEndpointProposalBody(res.Value))
}

func (a *ModelEndpointsAPI) listTeams(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("endpointId")
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]me.EndpointTeam, string, error) {
		return a.Svc.ListEndpointTeams(r.Context(), c, id, p)
	}, toEndpointTeamBody)
}

// endpointSecretValue decodes the write-only credential body; the value is never logged or returned.
func endpointSecretValue(r *http.Request) (domain.SecretValue, error) {
	var req struct {
		Value *string `json:"value"`
	}
	if err := Decode(r, &req); err != nil {
		return domain.SecretValue{}, err
	}
	if req.Value == nil {
		return domain.SecretValue{}, bad("value", "is required")
	}
	return domain.NewSecretValue("value", *req.Value)
}

func (a *ModelEndpointsAPI) putCredential(w http.ResponseWriter, r *http.Request) {
	a.putCredentialWith(w, r, func(c acting.Caller, v domain.SecretValue, expect *domain.Version) (me.Credential, error) {
		return a.Svc.PutCredential(r.Context(), c, r.PathValue("endpointId"), r.PathValue("scope"), v, expect)
	})
}

func (a *ModelEndpointsAPI) putTeamCredential(w http.ResponseWriter, r *http.Request) {
	a.putCredentialWith(w, r, func(c acting.Caller, v domain.SecretValue, expect *domain.Version) (me.Credential, error) {
		return a.Svc.PutTeamCredential(r.Context(), c, r.PathValue("teamId"), r.PathValue("endpointId"), v, expect)
	})
}

func (a *ModelEndpointsAPI) putCredentialWith(w http.ResponseWriter, r *http.Request, do func(acting.Caller, domain.SecretValue, *domain.Version) (me.Credential, error)) {
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
	v, err := endpointSecretValue(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cred, err := do(c, v, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, cred.Version)
	WriteJSON(w, http.StatusOK, toEndpointCredentialBody(cred))
}

func (a *ModelEndpointsAPI) rotateCredential(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := endpointSecretValue(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, scope := r.PathValue("endpointId"), r.PathValue("scope")
	idem, err := w2Idem(r, "RotateModelEndpointCredential", "", append([]byte(id+"\n"+scope+"\n"), raw...), func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toEndpointCredentialBody(v.(me.Credential)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.RotateCredential(r.Context(), c, id, scope, v, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Value.Version)
	WriteJSON(w, http.StatusOK, toEndpointCredentialBody(res.Value))
}

func (a *ModelEndpointsAPI) putFederation(w http.ResponseWriter, r *http.Request) {
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
		TenantID      string `json:"tenantId"`
		ClientID      string `json:"clientId"`
		ProjectNumber string `json:"projectNumber"`
		PoolID        string `json:"poolId"`
		ProviderID    string `json:"providerId"`
		ProjectID     string `json:"projectId"`
		Location      string `json:"location"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	f := me.Federation{TenantID: req.TenantID, ClientID: req.ClientID, ProjectNumber: req.ProjectNumber, PoolID: req.PoolID,
		ProviderID: req.ProviderID, ProjectID: req.ProjectID, Location: req.Location}
	out, err := a.Svc.PutFederation(r.Context(), c, r.PathValue("endpointId"), r.PathValue("teamId"), f, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, toEndpointFederationBody(out))
}

// maxStatementRows bounds a statement as the database does (afs.statement_rows_ok).
const maxStatementRows = 10000

type statementRowInput struct {
	TeamID    string       `json:"teamId"`
	Model     string       `json:"model"`
	AmountUSD domain.Money `json:"amountUsd"`
	Tokens    *int64       `json:"tokens"`
}

func (a *ModelEndpointsAPI) importStatement(w http.ResponseWriter, r *http.Request) {
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	st, err := parseStatement(r, raw)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id := r.PathValue("endpointId")
	created(w, r, "ImportProviderStatement", "", append([]byte(id+"\n"+r.URL.RawQuery+"\n"), raw...), toProviderStatementBody, nil,
		func(c acting.Caller, idem *acting.Idem) (me.Created[me.Statement], error) {
			return a.Svc.ImportStatement(r.Context(), c, id, st, idem)
		})
}

// parseStatement reads a statement as JSON (period, source and rows in the body) or as text/csv (period
// and source in the query; header teamId,model,amountUsd[,tokens]).
func parseStatement(r *http.Request, raw []byte) (me.Statement, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	qs := r.URL.Query()
	if mt != "text/csv" {
		if qs.Get("period") != "" || qs.Get("source") != "" {
			return me.Statement{}, bad("period", "period and source go in the body of a JSON statement")
		}
		var req struct {
			Period string              `json:"period"`
			Source string              `json:"source"`
			Rows   []statementRowInput `json:"rows"`
		}
		if err := Decode(r, &req); err != nil {
			return me.Statement{}, err
		}
		return statementOf(req.Period, req.Source, req.Rows)
	}
	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil {
		return me.Statement{}, bad("body", "a CSV statement starts with the header teamId,model,amountUsd[,tokens]")
	}
	cols := map[string]int{}
	for i, h := range header {
		cols[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	for _, k := range []string{"teamId", "model", "amountUsd"} {
		if _, ok := cols[k]; !ok {
			return me.Statement{}, bad("body", "the CSV header lacks "+k)
		}
	}
	tokCol, hasTok := cols["tokens"]
	var rows []statementRowInput
	for line := 2; ; line++ {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		field := "body.line" + strconv.Itoa(line)
		if err != nil || len(rec) < len(header) {
			return me.Statement{}, bad(field, "malformed CSV row")
		}
		if len(rows) == maxStatementRows {
			return me.Statement{}, bad("body", "at most 10000 rows")
		}
		amt, err := domain.ParseMoney(field+".amountUsd", strings.TrimSpace(rec[cols["amountUsd"]]))
		if err != nil {
			return me.Statement{}, err
		}
		row := statementRowInput{TeamID: strings.TrimSpace(rec[cols["teamId"]]), Model: strings.TrimSpace(rec[cols["model"]]), AmountUSD: amt}
		if hasTok && strings.TrimSpace(rec[tokCol]) != "" {
			n, err := strconv.ParseInt(strings.TrimSpace(rec[tokCol]), 10, 64)
			if err != nil {
				return me.Statement{}, bad(field+".tokens", "must be a whole number")
			}
			row.Tokens = &n
		}
		rows = append(rows, row)
	}
	return statementOf(qs.Get("period"), qs.Get("source"), rows)
}

func statementOf(period, source string, rows []statementRowInput) (me.Statement, error) {
	p, err := me.ParsePeriod(period)
	if err != nil {
		return me.Statement{}, err
	}
	if len(rows) > maxStatementRows {
		return me.Statement{}, bad("rows", "at most 10000 rows")
	}
	st := me.Statement{Period: p, Source: source, Rows: make([]me.StatementRow, 0, len(rows))}
	for _, row := range rows {
		st.Rows = append(st.Rows, me.StatementRow{TeamID: row.TeamID, Model: row.Model, AmountUSD: row.AmountUSD, Tokens: row.Tokens})
	}
	return st, nil
}

// ---- providers ----

func (a *ModelEndpointsAPI) listProviders(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ps, err := a.Svc.ListProviders(r.Context(), c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := struct {
		Items []inferenceProviderBody `json:"items"`
	}{Items: make([]inferenceProviderBody, 0, len(ps))}
	for _, p := range ps {
		out.Items = append(out.Items, toInferenceProviderBody(p))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *ModelEndpointsAPI) updateProvider(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	dry := false
	switch r.URL.Query().Get("dryRun") {
	case "", "false":
	case "true":
		dry = true
	default:
		WriteError(w, r, bad("dryRun", "must be true or false"))
		return
	}
	var req struct {
		Enabled           *bool   `json:"enabled"`
		TeamSelection     *string `json:"teamSelection"`
		Reason            *string `json:"reason"`
		FallbackModel     *string `json:"fallbackModel"`
		SelectForAllTeams *bool   `json:"selectForAllTeams"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := me.ProviderPatch{Enabled: req.Enabled, Reason: req.Reason, FallbackModel: req.FallbackModel, SelectForAllTeams: req.SelectForAllTeams != nil && *req.SelectForAllTeams}
	if req.TeamSelection != nil {
		ts := domain.TeamSelection(*req.TeamSelection)
		p.TeamSelection = &ts
	}
	out, err := a.Svc.UpdateProvider(r.Context(), c, r.PathValue("provider"), p, dry, v)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, domain.Version(out.Version))
	WriteJSON(w, http.StatusOK, toInferenceProviderBody(out))
}

// ---- team ----

func (a *ModelEndpointsAPI) putGateway(w http.ResponseWriter, r *http.Request) {
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
		Deployment           *string `json:"deployment"`
		MayAddModelEndpoints *bool   `json:"mayAddModelEndpoints"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := me.TeamGatewayPatch{MayAdd: req.MayAddModelEndpoints}
	if req.Deployment != nil {
		d := domain.GatewayDeployment(*req.Deployment)
		p.Deployment = &d
	}
	g, err := a.Svc.PutTeamGateway(r.Context(), c, r.PathValue("teamId"), p, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, g.Version)
	WriteJSON(w, http.StatusOK, toTeamGatewayBody(g))
}

func (a *ModelEndpointsAPI) listTeamEndpoints(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Svc.ListTeamEndpoints(r.Context(), c, r.PathValue("teamId"), r.URL.Query().Get("relation"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := teamModelEndpointListBody{Items: make([]teamModelEndpointBody, 0, len(l.Items)), NextToken: w2Next(a.Pages, r, c, l.Next),
		GatewayDeployment: string(l.Deployment), MayAddModelEndpoints: l.MayAdd}
	for _, te := range l.Items {
		out.Items = append(out.Items, toTeamModelEndpointBody(te))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *ModelEndpointsAPI) putSelections(w http.ResponseWriter, r *http.Request) {
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
		EndpointIDs *[]string `json:"endpointIds"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.EndpointIDs == nil {
		WriteError(w, r, bad("endpointIds", "is required"))
		return
	}
	res, err := a.Svc.PutSelections(r.Context(), c, r.PathValue("teamId"), *req.EndpointIDs, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toSelectionsBody(res))
}

func (a *ModelEndpointsAPI) createTeamEndpoint(w http.ResponseWriter, r *http.Request) {
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createEndpointRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	switch {
	case req.Provider != "":
		err = bad("provider", "unknown field")
	case req.CredentialMode != "":
		err = bad("credentialMode", "unknown field")
	case req.RequiresTerms:
		err = bad("requiresTerms", "unknown field")
	case req.UseProviderCost:
		err = bad("useProviderCost", "unknown field")
	case req.ExternalSystemID != "":
		err = bad("externalSystemId", "unknown field")
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	models, err := modelInputs(req.Models, true)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	in := me.NewEndpoint{Endpoint: req.endpoint(), Models: models}
	created(w, r, "CreateTeamModelEndpoint", teamID, raw, toModelEndpointBody, endpointVersion, func(c acting.Caller, idem *acting.Idem) (me.Created[me.Endpoint], error) {
		return a.Svc.CreateTeamEndpoint(r.Context(), c, teamID, in, idem)
	})
}

func (a *ModelEndpointsAPI) updateTeamEndpoint(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("teamId")
	a.patchEndpoint(w, r, teamID, func(c acting.Caller, p me.Patch, v domain.Version) (me.Endpoint, error) {
		return a.Svc.UpdateTeamEndpoint(r.Context(), c, teamID, r.PathValue("endpointId"), p, v)
	})
}

func (a *ModelEndpointsAPI) deleteTeamEndpoint(w http.ResponseWriter, r *http.Request) {
	a.deleteWith(w, r, func(c acting.Caller, v domain.Version) error {
		return a.Svc.DeleteTeamEndpoint(r.Context(), c, r.PathValue("teamId"), r.PathValue("endpointId"), v)
	})
}

func (a *ModelEndpointsAPI) putTeamModels(w http.ResponseWriter, r *http.Request) {
	a.putModelsWith(w, r, true, func(c acting.Caller, ms []me.ModelInput, v *domain.Version) (me.ModelList, domain.PricingMode, error) {
		l, err := a.Svc.PutTeamModels(r.Context(), c, r.PathValue("teamId"), r.PathValue("endpointId"), ms, v)
		return l, modeOf(l), err
	})
}

// ---- reports ----

func (a *ModelEndpointsAPI) report(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	qs := r.URL.Query()
	raw := me.RawReport{From: qs.Get("from"), To: qs.Get("to"), Range: qs.Get("range"), GroupBy: qs.Get("groupBy"), TeamID: qs.Get("teamId"),
		Route: qs.Get("route"), ModelID: qs.Get("modelId"), AgentID: qs.Get("agentId"), PoolID: qs.Get("poolId")}
	rep, err := a.Svc.InferenceReport(r.Context(), c, r.PathValue("teamId"), raw, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := inferenceReportBody{Range: reportRangeBody{From: w2Time(rep.From), To: w2Time(rep.To)}, GroupBy: rep.GroupBy,
		Rows: make([]inferenceRowBody, 0, len(rep.Rows)), Totals: toInferenceRowBody(rep.Totals), Notes: rep.Notes,
		NextToken: w2Next(a.Pages, r, c, rep.Next)}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	for _, row := range rep.Rows {
		out.Rows = append(out.Rows, toInferenceRowBody(row))
	}
	WriteJSON(w, http.StatusOK, out)
}
