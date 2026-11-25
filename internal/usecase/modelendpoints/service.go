package modelendpoints

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// Config is what the use cases need from the deployment.
type Config struct {
	// OIDCIssuer is the EKS cluster's OIDC issuer URL (Azure federation: the issuer to trust).
	OIDCIssuer string
	// Partition and AccountID name the AFS account (Google federation: the team gateway role to map).
	Partition string
	AccountID string
	// SharedGatewayURL is the AFS_GATEWAY_URL of teams on the shared gateway ("" when unknown).
	SharedGatewayURL string
}

// Service is the model endpoint use case set.
type Service struct {
	acting.Deps
	Repo    Repository
	Secrets secrets.Writer
	Config  Config
	// OnOrphan is called when a Secrets Manager secret created by a failed request could not be removed.
	OnOrphan func()
}

// Created is a create's result, or the stored response of a replayed one.
type Created[T any] struct {
	Value  T
	Replay *port.StoredResponse
}

// errDenied carries a contract reason out of a transaction; the denied audit is written outside it.
type errDenied struct {
	perm   authz.Permission
	reason string
	msg    string
}

func (e errDenied) Error() string { return e.msg }

func (s *Service) serviceCheck(ctx context.Context, c acting.Caller, action string, perm authz.Permission) error {
	if !authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", perm)
	}
	return nil
}

func (s *Service) teamCheck(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

// deny turns an errDenied returned by a transaction into the audited AccessDenied with its reason.
func (s *Service) deny(ctx context.Context, c acting.Caller, action, teamID string, err error) error {
	var d errDenied
	if !errors.As(err, &d) {
		return err
	}
	out := s.Denied(ctx, c, action, teamID, d.perm)
	var de *domain.Error
	if errors.As(out, &de) {
		de.Reason, de.Message = d.reason, d.msg
	}
	return out
}

func isServiceAdmin(c acting.Caller) bool {
	return authz.Authorize(c.Principal, authz.AdminModelEndpoints, authz.Resource{Scope: authz.ScopeService}).Allowed()
}

func refused(field, reason, msg string) *domain.Error {
	e := domain.Validation(domain.FieldError{Field: field, Reason: reason})
	e.Reason = reason
	if msg != "" {
		e.Message = msg
	}
	return e
}

func auditEndpoint(e domain.ModelEndpoint) map[string]any {
	return map[string]any{"id": e.ID, "teamId": e.TeamID, "slug": e.Slug, "displayName": e.DisplayName, "provider": e.Provider,
		"protocols": e.Protocols, "location": e.Location, "baseUrl": e.BaseURL, "authKind": e.AuthKind, "authHeader": e.AuthHeader,
		"credentialMode": e.CredentialMode, "pricingMode": e.PricingMode, "requiresTerms": e.RequiresTerms,
		"useProviderCost": e.UseProviderCost, "enabled": e.Enabled, "timeoutSeconds": e.TimeoutSeconds, "maxBodyMib": e.MaxBodyMiB,
		"owner": e.Owner, "purpose": e.Purpose, "metricsPort": e.MetricsPort, "externalSystemId": e.ExternalSystem}
}

func auditModels(ms []Model) []map[string]any {
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, map[string]any{"modelName": m.ModelName, "maxOutputTokens": m.MaxOutputTokens, "enabled": m.Enabled})
	}
	return out
}

func eventData(e domain.ModelEndpoint, extra map[string]any) map[string]any {
	d := map[string]any{"endpointId": e.ID, "slug": e.Slug, "provider": e.Provider}
	if e.TeamID != "" {
		d["teamId"] = e.TeamID
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// ListEndpoints implements ListModelEndpoints.
func (s *Service) ListEndpoints(ctx context.Context, c acting.Caller, f Filter, p acting.Page) ([]Endpoint, string, error) {
	if err := s.serviceCheck(ctx, c, "ListModelEndpoints", authz.AdminModelEndpoints); err != nil {
		return nil, "", err
	}
	if f.Location != "" && !slices.Contains([]string{"external", "corporate", "in_namespace"}, f.Location) {
		return nil, "", refused("location", "must be external, corporate or in_namespace", "")
	}
	if f.PricingMode != "" && !slices.Contains([]string{"per_token", "capacity", "free"}, f.PricingMode) {
		return nil, "", refused("pricingMode", "must be per_token, capacity or free", "")
	}
	p = p.Normalized()
	var rows []Endpoint
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListEndpoints(ctx, tx, f, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(e Endpoint) string { return e.ID })
	return page, next, nil
}

// GetEndpoint implements GetModelEndpoint.
func (s *Service) GetEndpoint(ctx context.Context, c acting.Caller, id string) (Endpoint, error) {
	if err := s.serviceCheck(ctx, c, "GetModelEndpoint", authz.AdminModelEndpoints); err != nil {
		return Endpoint{}, err
	}
	var out Endpoint
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.GetEndpoint(ctx, tx, id, false)
		return err
	})
	return out, err
}

// ModelInput is one model of a PUT models request; Price is set inline on team endpoints only.
type ModelInput struct {
	domain.EndpointModel
	Price *Price
}

func modelsOf(in []ModelInput) []domain.EndpointModel {
	out := make([]domain.EndpointModel, 0, len(in))
	for _, m := range in {
		out = append(out, m.EndpointModel)
	}
	return out
}

func validPrice(p Price, field string) error {
	if p.Input.IsNegative() || p.Output.IsNegative() || p.Fee.IsNegative() || (p.CachedInput != nil && p.CachedInput.IsNegative()) {
		return refused(field, "prices must not be negative", "")
	}
	return nil
}

// NewEndpoint is a create request; Models may be empty.
type NewEndpoint struct {
	Endpoint domain.ModelEndpoint
	Models   []ModelInput
}

func defaults(e *domain.ModelEndpoint) {
	if e.TimeoutSeconds == 0 {
		e.TimeoutSeconds = 300
	}
	if e.MaxBodyMiB == 0 {
		e.MaxBodyMiB = 20
	}
	if e.CredentialMode == "" {
		e.CredentialMode = domain.CredentialPerTeam
	}
}

// providerOn reads the provider switch; internal has none and is always on.
func (s *Service) providerOn(ctx context.Context, tx port.Tx, provider string) (bool, error) {
	if provider == domain.ProviderInternal {
		return true, nil
	}
	p, err := s.Repo.GetProvider(ctx, tx, provider, false)
	if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
		return false, refused("provider", "unknown provider", "provider "+provider+" is not in the provider policy")
	}
	if err != nil {
		return false, err
	}
	return p.Enabled, nil
}

func providerDisabled(provider string) *domain.Error {
	return refused("provider", domain.ReasonProviderDisabled, "provider "+provider+" is turned off by the provider policy")
}

// CreateEndpoint implements CreateModelEndpoint: a service endpoint, created off.
func (s *Service) CreateEndpoint(ctx context.Context, c acting.Caller, in NewEndpoint, idem *acting.Idem) (Created[Endpoint], error) {
	var res Created[Endpoint]
	if err := s.serviceCheck(ctx, c, "CreateModelEndpoint", authz.AdminModelEndpoints); err != nil {
		return res, err
	}
	e := in.Endpoint
	e.TeamID, e.Enabled, e.Builtin = "", false, false
	defaults(&e)
	derive := e.Provider == ""
	if derive {
		e.Provider = domain.ProviderOther // replaced from the host inside the transaction
	}
	if err := e.Validate(); err != nil {
		return res, err
	}
	if err := domain.ValidateEndpointModels(modelsOf(in.Models), "models"); err != nil {
		return res, err
	}
	for i, m := range in.Models {
		if m.Price != nil {
			return res, refused(fmt.Sprintf("models[%d].price", i), "service endpoint prices are set with CreateEndpointModelPrice", "")
		}
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[Endpoint]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		ps, err := s.providerList(ctx, tx)
		if err != nil {
			return err
		}
		e.Provider = serviceProvider(ps, e, derive)
		out, err := s.insert(ctx, tx, e, in.Models)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateModelEndpoint", kindEndpoint, out.ID, "", nil, auditEndpoint(out.ModelEndpoint))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EndpointLifecycleEvent(true), "", kindEndpoint, out.ID, eventData(out.ModelEndpoint, nil)); err != nil {
			return err
		}
		res.Value = out
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return res, err
}

func (s *Service) insert(ctx context.Context, tx port.Tx, e domain.ModelEndpoint, models []ModelInput) (Endpoint, error) {
	on, err := s.providerOn(ctx, tx, e.Provider)
	if err != nil {
		return Endpoint{}, err
	}
	if !on {
		return Endpoint{}, providerDisabled(e.Provider)
	}
	id, err := s.IDs.NewID("mep")
	if err != nil {
		return Endpoint{}, err
	}
	e.ID = id.String()
	if err := s.Repo.InsertEndpoint(ctx, tx, e); err != nil {
		return Endpoint{}, err
	}
	if len(models) > 0 {
		if err := s.Repo.PutModels(ctx, tx, e.ID, modelsOf(models)); err != nil {
			return Endpoint{}, err
		}
		if err := s.inlinePrices(ctx, tx, e.ID, models); err != nil {
			return Endpoint{}, err
		}
	}
	return s.Repo.GetEndpoint(ctx, tx, e.ID, false)
}

// inlinePrices writes the inline prices of a team endpoint's models, effective now.
func (s *Service) inlinePrices(ctx context.Context, tx port.Tx, endpointID string, models []ModelInput) error {
	for i, m := range models {
		if m.Price == nil {
			continue
		}
		if err := validPrice(*m.Price, fmt.Sprintf("models[%d].price", i)); err != nil {
			return err
		}
		if _, err := s.Repo.InsertPrice(ctx, tx, PriceRow{EndpointID: endpointID, ModelName: m.ModelName, EffectiveFrom: s.Now(),
			Price: *m.Price, Source: "manual"}); err != nil {
			return err
		}
	}
	return nil
}

// Patch is an endpoint update; nil members are kept. A pointer to the zero value clears a nullable member
// (caBundle, authHeader, externalSystemId, metricsPort).
type Patch struct {
	DisplayName, Description, BaseURL, CABundle, AuthHeader, Provider, Owner, Purpose, ExternalSystem *string
	AuthKind                                                                                          *domain.AuthKind
	PricingMode                                                                                       *domain.PricingMode
	Location                                                                                          *domain.EndpointLocation
	CredentialMode                                                                                    *domain.CredentialMode
	Protocols                                                                                         *[]domain.Protocol
	TimeoutSeconds, MaxBodyMiB, MetricsPort                                                           *int
	RequiresTerms, UseProviderCost, Enabled                                                           *bool
}

func set[T any](dst *T, src *T, changed *bool) {
	if src != nil {
		*dst = *src
		*changed = true
	}
}

// apply returns e with the patch applied; other reports a change of anything but enabled.
func (p Patch) apply(e domain.ModelEndpoint) (out domain.ModelEndpoint, other bool) {
	out = e
	set(&out.DisplayName, p.DisplayName, &other)
	set(&out.Description, p.Description, &other)
	set(&out.BaseURL, p.BaseURL, &other)
	set(&out.CABundle, p.CABundle, &other)
	set(&out.AuthHeader, p.AuthHeader, &other)
	set(&out.Provider, p.Provider, &other)
	set(&out.Owner, p.Owner, &other)
	set(&out.Purpose, p.Purpose, &other)
	set(&out.ExternalSystem, p.ExternalSystem, &other)
	set(&out.AuthKind, p.AuthKind, &other)
	set(&out.PricingMode, p.PricingMode, &other)
	set(&out.Location, p.Location, &other)
	set(&out.CredentialMode, p.CredentialMode, &other)
	set(&out.Protocols, p.Protocols, &other)
	set(&out.TimeoutSeconds, p.TimeoutSeconds, &other)
	set(&out.MaxBodyMiB, p.MaxBodyMiB, &other)
	set(&out.MetricsPort, p.MetricsPort, &other)
	set(&out.RequiresTerms, p.RequiresTerms, &other)
	set(&out.UseProviderCost, p.UseProviderCost, &other)
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	return out, other
}

// UpdateEndpoint implements UpdateModelEndpoint (service endpoints; If-Match required).
func (s *Service) UpdateEndpoint(ctx context.Context, c acting.Caller, id string, p Patch, expect domain.Version) (Endpoint, error) {
	if err := s.serviceCheck(ctx, c, "UpdateModelEndpoint", authz.AdminModelEndpoints); err != nil {
		return Endpoint{}, err
	}
	if p.Location != nil && *p.Location == domain.LocationInNamespace {
		return Endpoint{}, refused("location", "in_namespace is for team endpoints only", "")
	}
	var out Endpoint
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetEndpoint(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.IsTeam() {
			return domain.NotFound("model endpoint")
		}
		out, err = s.update(ctx, tx, c, "UpdateModelEndpoint", cur, p, expect)
		return err
	})
	return out, err
}

func (s *Service) update(ctx context.Context, tx port.Tx, c acting.Caller, action string, cur Endpoint, p Patch, expect domain.Version) (Endpoint, error) {
	if err := domain.CheckVersion(cur.Version(), expect); err != nil {
		return Endpoint{}, err
	}
	next, other := p.apply(cur.ModelEndpoint)
	if err := next.Validate(); err != nil {
		return Endpoint{}, err
	}
	if next.Enabled || next.Provider != cur.Provider {
		on, err := s.providerOn(ctx, tx, next.Provider)
		if err != nil {
			return Endpoint{}, err
		}
		if !on {
			return Endpoint{}, providerDisabled(next.Provider)
		}
	}
	if err := s.Repo.UpdateEndpoint(ctx, tx, next, expect); err != nil {
		return Endpoint{}, err
	}
	out, err := s.Repo.GetEndpoint(ctx, tx, cur.ID, false)
	if err != nil {
		return Endpoint{}, err
	}
	if err := s.Audit.Write(ctx, tx, c.Record(action, kindEndpoint, cur.ID, cur.TeamID, auditEndpoint(cur.ModelEndpoint), auditEndpoint(out.ModelEndpoint))); err != nil {
		return Endpoint{}, err
	}
	events := domain.EndpointChangeEvents(cur.ModelEndpoint, out.ModelEndpoint, other)
	if len(events) == 0 {
		events = []domain.EventType{domain.EventModelEndpointUpdated}
	}
	for _, ev := range events {
		if err := s.Emit(ctx, tx, c, ev, cur.TeamID, kindEndpoint, cur.ID, eventData(out.ModelEndpoint, nil)); err != nil {
			return Endpoint{}, err
		}
	}
	return out, nil
}

// DeleteEndpoint implements DeleteModelEndpoint: refused while enabled (409 endpoint_enabled).
func (s *Service) DeleteEndpoint(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.serviceCheck(ctx, c, "DeleteModelEndpoint", authz.AdminModelEndpoints); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetEndpoint(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.IsTeam() {
			return domain.NotFound("model endpoint")
		}
		return s.delete(ctx, tx, c, "DeleteModelEndpoint", cur, expect)
	})
}

func (s *Service) delete(ctx context.Context, tx port.Tx, c acting.Caller, action string, cur Endpoint, expect domain.Version) error {
	if err := domain.CheckVersion(cur.Version(), expect); err != nil {
		return err
	}
	if cur.Enabled {
		return domain.Conflict(domain.ReasonEndpointEnabled, "switch the endpoint off before deleting it", nil)
	}
	if cur.Builtin {
		return domain.Conflict("invalid_state", "a built-in endpoint cannot be deleted; keep it switched off", nil)
	}
	if err := s.Repo.DeleteEndpoint(ctx, tx, cur.ID, expect); err != nil {
		return err
	}
	if err := s.Audit.Write(ctx, tx, c.Record(action, kindEndpoint, cur.ID, cur.TeamID, auditEndpoint(cur.ModelEndpoint), nil)); err != nil {
		return err
	}
	return s.Emit(ctx, tx, c, domain.EndpointLifecycleEvent(false), cur.TeamID, kindEndpoint, cur.ID, eventData(cur.ModelEndpoint, nil))
}

// ModelList is an endpoint's models with the endpoint's version.
type ModelList struct {
	EndpointID string
	Models     []Model
	Version    domain.Version
}

// PutModels implements PutEndpointModels. If-Match is required once the endpoint lists models.
func (s *Service) PutModels(ctx context.Context, c acting.Caller, id string, models []ModelInput, expect *domain.Version) (ModelList, error) {
	if err := s.serviceCheck(ctx, c, "PutEndpointModels", authz.AdminModelEndpoints); err != nil {
		return ModelList{}, err
	}
	for i, m := range models {
		if m.Price != nil {
			return ModelList{}, refused(fmt.Sprintf("models[%d].price", i), "service endpoint prices are set with CreateEndpointModelPrice", "")
		}
	}
	var out ModelList
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetEndpoint(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.IsTeam() {
			return domain.NotFound("model endpoint")
		}
		out, err = s.putModels(ctx, tx, c, "PutEndpointModels", cur, models, expect)
		return err
	})
	return out, err
}

func (s *Service) putModels(ctx context.Context, tx port.Tx, c acting.Caller, action string, cur Endpoint, models []ModelInput, expect *domain.Version) (ModelList, error) {
	if err := domain.ValidateEndpointModels(modelsOf(models), "models"); err != nil {
		return ModelList{}, err
	}
	switch {
	case expect != nil:
		if err := domain.CheckVersion(cur.Version(), *expect); err != nil {
			return ModelList{}, err
		}
	case len(cur.Models) > 0:
		return ModelList{}, domain.PreconditionRequired()
	}
	if err := s.Repo.PutModels(ctx, tx, cur.ID, modelsOf(models)); err != nil {
		return ModelList{}, err
	}
	if err := s.inlinePrices(ctx, tx, cur.ID, models); err != nil {
		return ModelList{}, err
	}
	after, err := s.Repo.GetEndpoint(ctx, tx, cur.ID, false)
	if err != nil {
		return ModelList{}, err
	}
	if err := s.Audit.Write(ctx, tx, c.Record(action, kindModels, cur.ID, cur.TeamID, auditModels(cur.Models), auditModels(after.Models))); err != nil {
		return ModelList{}, err
	}
	if err := s.Emit(ctx, tx, c, domain.EventModelEndpointUpdated, cur.TeamID, kindEndpoint, cur.ID,
		eventData(after.ModelEndpoint, map[string]any{"models": len(after.Models)})); err != nil {
		return ModelList{}, err
	}
	return ModelList{EndpointID: cur.ID, Models: after.Models, Version: after.Version()}, nil
}

// providerList returns the provider switches with hosts, other left out (it matches no host).
func (s *Service) providerList(ctx context.Context, tx port.Tx) ([]domain.InferenceProvider, error) {
	providers, err := s.Repo.ListProviders(ctx, tx)
	if err != nil {
		return nil, err
	}
	ps := make([]domain.InferenceProvider, 0, len(providers))
	for _, p := range providers {
		if p.Provider != domain.ProviderOther {
			ps = append(ps, p.InferenceProvider)
		}
	}
	return ps, nil
}

// serviceProvider mirrors the database rule for a service endpoint: a listed provider's host decides;
// otherwise the provider given, or internal for a corporate endpoint and other for an external one.
func serviceProvider(ps []domain.InferenceProvider, e domain.ModelEndpoint, derive bool) string {
	if e.Location == domain.LocationInNamespace {
		return domain.ProviderInternal
	}
	if d := domain.ProviderForHost(ps, e.Host()); d != domain.ProviderOther {
		return d
	}
	if !derive {
		return e.Provider
	}
	if e.Location == domain.LocationCorporate {
		return domain.ProviderInternal
	}
	return domain.ProviderOther
}
