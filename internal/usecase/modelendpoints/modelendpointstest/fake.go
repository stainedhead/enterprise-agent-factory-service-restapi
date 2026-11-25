// Package modelendpointstest is an in-memory modelendpoints.Repository for tests. It applies the
// database rules the use cases rely on (version checks, provider derivation is left to the caller) and
// nothing more.
package modelendpointstest

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo is the fake. Errs fails the named method once set.
type Repo struct {
	Now        time.Time
	Endpoints  map[string]modelendpoints.Endpoint
	Prices     []modelendpoints.PriceRow
	Proposals  map[string]modelendpoints.Proposal
	Creds      map[string]modelendpoints.Credential // endpoint/scope
	Feds       map[string]modelendpoints.Federation // endpoint/team
	Statements []modelendpoints.Statement
	Providers  map[string]modelendpoints.Provider
	Teams      map[string]modelendpoints.TeamGateway
	Selections map[string][]modelendpoints.Selection // by team
	Default    string
	Report     modelendpoints.Report
	LastQuery  modelendpoints.ReportQuery
	Impact     modelendpoints.Impact
	// Admin is the selected_by Select records.
	Admin bool
	Errs  map[string]error
}

// New returns an empty Repo with the seeded providers of migration 000023.
func New(now time.Time) *Repo {
	r := &Repo{Now: now, Endpoints: map[string]modelendpoints.Endpoint{}, Proposals: map[string]modelendpoints.Proposal{},
		Creds: map[string]modelendpoints.Credential{}, Feds: map[string]modelendpoints.Federation{}, Providers: map[string]modelendpoints.Provider{},
		Teams: map[string]modelendpoints.TeamGateway{}, Selections: map[string][]modelendpoints.Selection{}, Errs: map[string]error{}}
	for _, p := range []struct {
		key, host string
		on        bool
		sel       domain.TeamSelection
	}{
		{"bedrock", "bedrock-runtime.*.amazonaws.com", true, domain.SelectionTeamAdmin},
		{"openai", "api.openai.com", true, domain.SelectionTeamAdmin},
		{"openrouter", "openrouter.ai", true, domain.SelectionTeamAdmin},
		{"nous", "inference-api.nousresearch.com", true, domain.SelectionTeamAdmin},
		{"opencode", "opencode.ai", true, domain.SelectionTeamAdmin},
		{"azure", "*.openai.azure.com", false, domain.SelectionServiceAdmin},
		{"other", "", true, domain.SelectionTeamAdmin},
	} {
		var hosts []string
		if p.host != "" {
			hosts = []string{p.host}
		}
		r.Providers[p.key] = modelendpoints.Provider{InferenceProvider: domain.InferenceProvider{Provider: p.key, DisplayName: p.key, Hosts: hosts,
			Enabled: p.on, TeamSelection: p.sel, Version: 1}}
	}
	return r
}

func (r *Repo) err(name string) error { return r.Errs[name] }

var _ modelendpoints.Repository = (*Repo)(nil)

func (r *Repo) view(e modelendpoints.Endpoint) modelendpoints.Endpoint {
	p, ok := r.Providers[e.Provider]
	e.ProviderOn = e.Provider == domain.ProviderInternal || (ok && p.Enabled)
	if e.Health.State == "" {
		e.Health.State = domain.RouteHealthUnknown
	}
	for i, m := range e.Models {
		m.Price, m.PriceState = nil, modelendpoints.PriceNone
		if e.PricingMode != domain.PricingPerToken {
			m.PriceState = modelendpoints.PriceNotRequired
		}
		for _, pr := range r.Prices {
			if pr.EndpointID == e.ID && pr.ModelName == m.ModelName && !pr.EffectiveFrom.After(r.Now) {
				price := pr.Price
				m.Price, m.PriceState = &price, modelendpoints.PriceApproved
			}
		}
		e.Models[i] = m
	}
	n := 0
	for _, sels := range r.Selections {
		for _, s := range sels {
			if s.EndpointID == e.ID {
				n++
			}
		}
	}
	e.TeamsSelected = n
	return e
}

// ListEndpoints implements modelendpoints.Repository.
func (r *Repo) ListEndpoints(_ context.Context, _ port.Tx, f modelendpoints.Filter, after string, limit int) ([]modelendpoints.Endpoint, error) {
	var out []modelendpoints.Endpoint
	for _, e := range r.sorted() {
		if e.ID <= after || (e.IsTeam() && !f.IncludeTeams) || (f.Provider != "" && e.Provider != f.Provider) ||
			(f.Location != "" && string(e.Location) != f.Location) || (f.PricingMode != "" && string(e.PricingMode) != f.PricingMode) {
			continue
		}
		out = append(out, r.view(e))
		if len(out) == limit {
			break
		}
	}
	return out, r.err("ListEndpoints")
}

func (r *Repo) sorted() []modelendpoints.Endpoint {
	out := make([]modelendpoints.Endpoint, 0, len(r.Endpoints))
	for _, e := range r.Endpoints {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetEndpoint implements modelendpoints.Repository.
func (r *Repo) GetEndpoint(_ context.Context, _ port.Tx, id string, _ bool) (modelendpoints.Endpoint, error) {
	if err := r.err("GetEndpoint"); err != nil {
		return modelendpoints.Endpoint{}, err
	}
	e, ok := r.Endpoints[id]
	if !ok {
		return e, domain.NotFound("model endpoint")
	}
	e.Models = slices.Clone(e.Models)
	return r.view(e), nil
}

// InsertEndpoint implements modelendpoints.Repository.
func (r *Repo) InsertEndpoint(_ context.Context, _ port.Tx, e domain.ModelEndpoint) error {
	if err := r.err("InsertEndpoint"); err != nil {
		return err
	}
	for _, o := range r.Endpoints {
		if o.Slug == e.Slug && (o.TeamID == "" || e.TeamID == "" || o.TeamID == e.TeamID) {
			return domain.Conflict(domain.ReasonSlugTaken, "route "+e.Slug+" is taken", nil)
		}
	}
	e.Version, e.CreatedAt, e.UpdatedAt = 1, r.Now, r.Now
	r.Endpoints[e.ID] = modelendpoints.Endpoint{ModelEndpoint: e}
	return nil
}

// UpdateEndpoint implements modelendpoints.Repository.
func (r *Repo) UpdateEndpoint(_ context.Context, _ port.Tx, e domain.ModelEndpoint, expect domain.Version) error {
	if err := r.err("UpdateEndpoint"); err != nil {
		return err
	}
	cur := r.Endpoints[e.ID]
	if domain.Version(cur.ModelEndpoint.Version) != expect {
		return domain.PreconditionFailed()
	}
	e.Version = cur.ModelEndpoint.Version + 1
	cur.ModelEndpoint = e
	r.Endpoints[e.ID] = cur
	return nil
}

// DeleteEndpoint implements modelendpoints.Repository.
func (r *Repo) DeleteEndpoint(_ context.Context, _ port.Tx, id string, _ domain.Version) error {
	if err := r.err("DeleteEndpoint"); err != nil {
		return err
	}
	delete(r.Endpoints, id)
	return nil
}

// PutModels implements modelendpoints.Repository.
func (r *Repo) PutModels(_ context.Context, _ port.Tx, id string, ms []domain.EndpointModel) error {
	if err := r.err("PutModels"); err != nil {
		return err
	}
	e := r.Endpoints[id]
	e.Models = nil
	for _, m := range ms {
		e.Models = append(e.Models, modelendpoints.Model{EndpointModel: m})
	}
	e.ModelEndpoint.Version++
	r.Endpoints[id] = e
	return nil
}

// InsertPrice implements modelendpoints.Repository.
func (r *Repo) InsertPrice(_ context.Context, _ port.Tx, p modelendpoints.PriceRow) (modelendpoints.PriceRow, error) {
	if err := r.err("InsertPrice"); err != nil {
		return p, err
	}
	p.ApprovedBy, p.CreatedAt = "usr_root", r.Now
	r.Prices = append(r.Prices, p)
	return p, nil
}

// ListProposals implements modelendpoints.Repository.
func (r *Repo) ListProposals(_ context.Context, _ port.Tx, state, endpointID, after string, limit int) ([]modelendpoints.Proposal, error) {
	var ids []string
	for id := range r.Proposals {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	var out []modelendpoints.Proposal
	for _, id := range ids {
		p := r.Proposals[id]
		if (state != "" && p.State != state) || (endpointID != "" && p.EndpointID != endpointID) || (after != "" && id >= after) {
			continue
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out, r.err("ListProposals")
}

// GetProposal implements modelendpoints.Repository.
func (r *Repo) GetProposal(_ context.Context, _ port.Tx, id string, _ bool) (modelendpoints.Proposal, error) {
	p, ok := r.Proposals[id]
	if !ok {
		return p, domain.NotFound("price proposal")
	}
	return p, nil
}

// DecideProposal implements modelendpoints.Repository.
func (r *Repo) DecideProposal(_ context.Context, _ port.Tx, id, state, note string) (modelendpoints.Proposal, error) {
	if err := r.err("DecideProposal"); err != nil {
		return modelendpoints.Proposal{}, err
	}
	p := r.Proposals[id]
	p.State = state
	if note != "" {
		p.Note = note
	}
	t, by := r.Now, "usr_root"
	p.DecidedAt, p.DecidedBy = &t, &by
	r.Proposals[id] = p
	if state == modelendpoints.ProposalApproved {
		eff := p.EffectiveFrom
		if eff.Before(r.Now) {
			eff = r.Now
		}
		r.Prices = append(r.Prices, modelendpoints.PriceRow{EndpointID: p.EndpointID, ModelName: p.ModelName, EffectiveFrom: eff, Price: p.Price, Source: p.Source})
	}
	return p, nil
}

// ListEndpointTeams implements modelendpoints.Repository.
func (r *Repo) ListEndpointTeams(_ context.Context, _ port.Tx, e modelendpoints.Endpoint, after string, limit int) ([]modelendpoints.EndpointTeam, error) {
	var ids []string
	for id := range r.Teams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []modelendpoints.EndpointTeam
	for _, id := range ids {
		if id <= after || (e.IsTeam() && e.TeamID != id) {
			continue
		}
		t := r.Teams[id]
		et := modelendpoints.EndpointTeam{TeamID: id, GatewayDeployment: t.Deployment, CredentialState: modelendpoints.StateNotRequired,
			FederationState: modelendpoints.StateNotRequired}
		for _, s := range r.Selections[id] {
			if s.EndpointID == e.ID {
				et.Selected, et.SelectedBy = true, s.SelectedBy
			}
		}
		out = append(out, et)
		if len(out) == limit {
			break
		}
	}
	return out, r.err("ListEndpointTeams")
}

// CredentialPath implements modelendpoints.Repository.
func (r *Repo) CredentialPath(_ context.Context, _ port.Tx, e modelendpoints.Endpoint, scope string) (string, error) {
	if err := r.err("CredentialPath"); err != nil {
		return "", err
	}
	if scope == domain.SharedCredentialScope {
		return "afs/_service/gateway/" + e.Slug, nil
	}
	return "afs/" + r.Teams[scope].Namespace + "/_gateway/" + e.Slug, nil
}

// GetCredential implements modelendpoints.Repository.
func (r *Repo) GetCredential(_ context.Context, _ port.Tx, endpointID, scope string) (modelendpoints.Credential, error) {
	if err := r.err("GetCredential"); err != nil {
		return modelendpoints.Credential{}, err
	}
	c, ok := r.Creds[endpointID+"/"+scope]
	if !ok {
		return c, domain.NotFound("credential")
	}
	return c, nil
}

// PutCredential implements modelendpoints.Repository.
func (r *Repo) PutCredential(_ context.Context, _ port.Tx, c modelendpoints.Credential) (modelendpoints.Credential, error) {
	if err := r.err("PutCredential"); err != nil {
		return c, err
	}
	k := c.EndpointID + "/" + c.Scope
	t := r.Now
	c.State, c.RotatedAt, c.Version = modelendpoints.StateSet, &t, r.Creds[k].Version+1
	r.Creds[k] = c
	return c, nil
}

// GetFederation implements modelendpoints.Repository.
func (r *Repo) GetFederation(_ context.Context, _ port.Tx, endpointID, teamID string) (modelendpoints.Federation, error) {
	f, ok := r.Feds[endpointID+"/"+teamID]
	if !ok {
		return f, domain.NotFound("federation")
	}
	return f, r.err("GetFederation")
}

// PutFederation implements modelendpoints.Repository.
func (r *Repo) PutFederation(_ context.Context, _ port.Tx, f modelendpoints.Federation) (modelendpoints.Federation, error) {
	if err := r.err("PutFederation"); err != nil {
		return f, err
	}
	k := f.EndpointID + "/" + f.TeamID
	f.Status, f.Version = modelendpoints.FedPending, r.Feds[k].Version+1
	r.Feds[k] = f
	return f, nil
}

// InsertStatement implements modelendpoints.Repository.
func (r *Repo) InsertStatement(_ context.Context, _ port.Tx, s modelendpoints.Statement) (modelendpoints.Statement, error) {
	for _, o := range r.Statements {
		if o.EndpointID == s.EndpointID && o.Period.Equal(s.Period) && o.Source == s.Source {
			return s, domain.Conflict("duplicate", "a statement for the period exists", nil)
		}
	}
	s.ImportedBy, s.ImportedAt = "usr_root", r.Now
	r.Statements = append(r.Statements, s)
	return s, r.err("InsertStatement")
}

// ListProviders implements modelendpoints.Repository.
func (r *Repo) ListProviders(_ context.Context, _ port.Tx) ([]modelendpoints.Provider, error) {
	var keys []string
	for k := range r.Providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]modelendpoints.Provider, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.Providers[k])
	}
	return out, r.err("ListProviders")
}

// GetProvider implements modelendpoints.Repository.
func (r *Repo) GetProvider(_ context.Context, _ port.Tx, provider string, _ bool) (modelendpoints.Provider, error) {
	p, ok := r.Providers[provider]
	if !ok {
		return p, domain.NotFound("inference provider")
	}
	return p, r.err("GetProvider")
}

// UpdateProvider implements modelendpoints.Repository. It applies the Bedrock-off guard of the database.
func (r *Repo) UpdateProvider(_ context.Context, _ port.Tx, p domain.InferenceProvider, expect domain.Version) error {
	if err := r.err("UpdateProvider"); err != nil {
		return err
	}
	cur := r.Providers[p.Provider]
	if domain.Version(cur.Version) != expect {
		return domain.PreconditionFailed()
	}
	if cur.Enabled && !p.Enabled && p.Provider == domain.ProviderBedrock && !strings.Contains(r.Default, "/") {
		e := domain.Validation(domain.FieldError{Field: "fallbackModel", Reason: domain.ReasonNoFallbackModel})
		e.Reason = domain.ReasonNoFallbackModel
		return e
	}
	p.Version = cur.Version + 1
	cur.InferenceProvider = p
	r.Providers[p.Provider] = cur
	return nil
}

// ProviderImpact implements modelendpoints.Repository.
func (r *Repo) ProviderImpact(_ context.Context, _ port.Tx, _ string, fallback string) (modelendpoints.Impact, error) {
	im := r.Impact
	im.FallbackModel = fallback
	return im, r.err("ProviderImpact")
}

// SelectForAllTeams implements modelendpoints.Repository.
func (r *Repo) SelectForAllTeams(_ context.Context, _ port.Tx, endpointID string) (int, error) {
	n := 0
	for id := range r.Teams {
		if !slices.ContainsFunc(r.Selections[id], func(s modelendpoints.Selection) bool { return s.EndpointID == endpointID }) {
			r.Selections[id] = append(r.Selections[id], modelendpoints.Selection{EndpointID: endpointID, Slug: r.Endpoints[endpointID].Slug, SelectedBy: "service_admin"})
			n++
		}
	}
	return n, r.err("SelectForAllTeams")
}

// SetServiceDefaultModel implements modelendpoints.Repository.
func (r *Repo) SetServiceDefaultModel(_ context.Context, _ port.Tx, v string) error {
	r.Default = v
	return r.err("SetServiceDefaultModel")
}

// ServiceEndpointBySlug implements modelendpoints.Repository.
func (r *Repo) ServiceEndpointBySlug(ctx context.Context, tx port.Tx, slug string) (modelendpoints.Endpoint, error) {
	for _, e := range r.Endpoints {
		if e.Slug == slug && !e.IsTeam() {
			return r.GetEndpoint(ctx, tx, e.ID, false)
		}
	}
	return modelendpoints.Endpoint{}, domain.NotFound("model endpoint")
}

// TeamGateway implements modelendpoints.Repository.
func (r *Repo) TeamGateway(_ context.Context, _ port.Tx, teamID string, _ bool) (modelendpoints.TeamGateway, error) {
	t, ok := r.Teams[teamID]
	if !ok {
		return t, domain.NotFound("team")
	}
	t.TeamID = teamID
	return t, r.err("TeamGateway")
}

// PutTeamGateway implements modelendpoints.Repository.
func (r *Repo) PutTeamGateway(_ context.Context, _ port.Tx, teamID string, d domain.GatewayDeployment, may, mayMcp bool, placement string, expect domain.Version) error {
	if err := r.err("PutTeamGateway"); err != nil {
		return err
	}
	t := r.Teams[teamID]
	if t.Version != expect {
		return domain.PreconditionFailed()
	}
	t.Deployment, t.MayAdd, t.Version = d, may, t.Version+1
	t.MayAddMcp, t.McpPlacementStored, t.McpPlacement = mayMcp, placement, placement
	if placement == "" {
		t.McpPlacement = "combined"
	}
	r.Teams[teamID] = t
	return nil
}

func (r *Repo) teamView(teamID string, e modelendpoints.Endpoint) (modelendpoints.TeamEndpoint, bool) {
	te := modelendpoints.TeamEndpoint{Endpoint: r.view(e), CredentialState: modelendpoints.StateNotRequired, FederationState: modelendpoints.StateNotRequired}
	switch {
	case e.TeamID == teamID:
		te.Relation = modelendpoints.RelationOwn
	case e.IsTeam():
		return te, false
	default:
		te.Relation = modelendpoints.RelationAvailable
		for _, s := range r.Selections[teamID] {
			if s.EndpointID == e.ID {
				te.Relation, te.SelectedBy = modelendpoints.RelationSelected, s.SelectedBy
			}
		}
	}
	te.SelectableBy = r.Providers[e.Provider].TeamSelection
	if e.RequiresTerms {
		te.SelectableBy = domain.SelectionServiceAdmin
	}
	if e.AuthKind.NeedsCredential() {
		scope := teamID
		if !e.IsTeam() && e.CredentialMode == domain.CredentialShared {
			scope = domain.SharedCredentialScope
		}
		te.CredentialState = modelendpoints.StateMissing
		if _, ok := r.Creds[e.ID+"/"+scope]; ok {
			te.CredentialState = modelendpoints.StateSet
		}
	}
	if e.AuthKind.Federated() {
		te.FederationState = modelendpoints.FedPending
		if f, ok := r.Feds[e.ID+"/"+teamID]; ok {
			te.FederationState = f.Status
		}
	}
	return te, true
}

// ListTeamEndpoints implements modelendpoints.Repository.
func (r *Repo) ListTeamEndpoints(_ context.Context, _ port.Tx, teamID, relation, after string, limit int) ([]modelendpoints.TeamEndpoint, error) {
	var out []modelendpoints.TeamEndpoint
	for _, e := range r.sorted() {
		te, ok := r.teamView(teamID, e)
		if !ok || e.ID <= after || (relation != "" && te.Relation != relation) {
			continue
		}
		out = append(out, te)
		if len(out) == limit {
			break
		}
	}
	return out, r.err("ListTeamEndpoints")
}

// TeamEndpoint implements modelendpoints.Repository.
func (r *Repo) TeamEndpoint(_ context.Context, _ port.Tx, teamID, endpointID string) (modelendpoints.TeamEndpoint, error) {
	e, ok := r.Endpoints[endpointID]
	if !ok {
		return modelendpoints.TeamEndpoint{}, domain.NotFound("model endpoint")
	}
	te, ok := r.teamView(teamID, e)
	if !ok {
		return te, domain.NotFound("model endpoint")
	}
	return te, r.err("TeamEndpoint")
}

// TeamSelections implements modelendpoints.Repository.
func (r *Repo) TeamSelections(_ context.Context, _ port.Tx, teamID string) ([]modelendpoints.Selection, error) {
	out := slices.Clone(r.Selections[teamID])
	sort.Slice(out, func(i, j int) bool { return out[i].EndpointID < out[j].EndpointID })
	return out, r.err("TeamSelections")
}

// Select implements modelendpoints.Repository.
func (r *Repo) Select(_ context.Context, _ port.Tx, teamID, endpointID string) error {
	by := "team_admin"
	if r.Admin {
		by = "service_admin"
	}
	r.Selections[teamID] = append(r.Selections[teamID], modelendpoints.Selection{EndpointID: endpointID, Slug: r.Endpoints[endpointID].Slug, SelectedBy: by})
	return r.err("Select")
}

// Deselect implements modelendpoints.Repository.
func (r *Repo) Deselect(_ context.Context, _ port.Tx, teamID, endpointID string) error {
	r.Selections[teamID] = slices.DeleteFunc(r.Selections[teamID], func(s modelendpoints.Selection) bool { return s.EndpointID == endpointID })
	return r.err("Deselect")
}

// InferenceReport implements modelendpoints.Repository.
func (r *Repo) InferenceReport(_ context.Context, _ port.Tx, q modelendpoints.ReportQuery) (modelendpoints.Report, error) {
	r.LastQuery = q
	out := r.Report
	if q.Offset < len(out.Rows) {
		out.Rows = out.Rows[q.Offset:]
	} else {
		out.Rows = nil
	}
	if len(out.Rows) > q.Limit {
		out.Rows = out.Rows[:q.Limit]
	}
	return out, r.err("InferenceReport")
}
