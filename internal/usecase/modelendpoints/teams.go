package modelendpoints

import (
	"context"
	"fmt"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MaxSelections is the most service endpoints one PutTeamModelEndpoints request names.
const MaxSelections = 200

// OffReason is why a route a team sees cannot be called, "" when it can: the first of provider off,
// endpoint off, not selected, credential missing and federation not ready.
func OffReason(t TeamEndpoint) string {
	switch {
	case !t.ProviderOn:
		return OffProviderDisabled
	case !t.Enabled:
		return OffEndpointDisabled
	case t.Relation == RelationAvailable:
		return OffNotSelected
	case t.CredentialState == StateMissing:
		return OffCredentialMissing
	case t.FederationState == FedPending || t.FederationState == FedFailing:
		return OffFederationNotReady
	}
	return ""
}

// TeamList is a page of a team's routes with the team's gateway settings.
type TeamList struct {
	Items      []TeamEndpoint
	Next       string
	Deployment domain.GatewayDeployment
	MayAdd     bool
}

// ListTeamEndpoints implements ListTeamModelEndpoints; relation filters when not empty.
func (s *Service) ListTeamEndpoints(ctx context.Context, c acting.Caller, teamID, relation string, p acting.Page) (TeamList, error) {
	if err := s.teamCheck(ctx, c, "ListTeamModelEndpoints", teamID, authz.ModelEndpointsRead); err != nil {
		return TeamList{}, err
	}
	if relation != "" && !slices.Contains([]string{RelationSelected, RelationOwn, RelationAvailable}, relation) {
		return TeamList{}, refused("relation", "must be selected, own or available", "")
	}
	p = p.Normalized()
	var out TeamList
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		g, err := s.Repo.TeamGateway(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		rows, err := s.Repo.ListTeamEndpoints(ctx, tx, teamID, relation, p.Cursor, p.Limit+1)
		if err != nil {
			return err
		}
		out.Items, out.Next = acting.Slice(rows, p.Limit, func(r TeamEndpoint) string { return r.ID })
		out.Deployment, out.MayAdd = g.Deployment, g.MayAdd
		return nil
	})
	for i := range out.Items {
		out.Items[i].OffReason = OffReason(out.Items[i])
	}
	return out, err
}

// SelectionsResult is the team's selections after a PutTeamModelEndpoints.
type SelectionsResult struct {
	TeamID     string
	Selections []Selection
}

// EndpointIDs are the selected endpoint ids.
func (r SelectionsResult) EndpointIDs() []string {
	out := make([]string, 0, len(r.Selections))
	for _, s := range r.Selections {
		out = append(out, s.EndpointID)
	}
	return out
}

func validSelection(ids []string) error {
	if len(ids) > MaxSelections {
		return refused("endpointIds", fmt.Sprintf("at most %d endpoints", MaxSelections), "")
	}
	seen := map[string]bool{}
	var fe []domain.FieldError
	for i, id := range ids {
		f := fmt.Sprintf("endpointIds[%d]", i)
		if _, err := domain.ParseID("mep", id); err != nil {
			fe = append(fe, domain.FieldError{Field: f, Reason: "must be a model endpoint id"})
		} else if seen[id] {
			fe = append(fe, domain.FieldError{Field: f, Reason: "is listed twice"})
		}
		seen[id] = true
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// PutSelections implements PutTeamModelEndpoints: the service endpoints the team opts in to; one left out
// is deselected. An extra-terms endpoint or one whose provider is selected by service admins needs a
// service admin, and so does removing a selection a service admin made (403 needs_service_admin).
func (s *Service) PutSelections(ctx context.Context, c acting.Caller, teamID string, ids []string, expect *domain.Version) (SelectionsResult, error) {
	if err := s.teamCheck(ctx, c, "PutTeamModelEndpoints", teamID, authz.ModelEndpointsWrite); err != nil {
		return SelectionsResult{}, err
	}
	if err := validSelection(ids); err != nil {
		return SelectionsResult{}, err
	}
	admin := isServiceAdmin(c)
	var out SelectionsResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		g, err := s.Repo.TeamGateway(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(g.Version, *expect); err != nil {
				return err
			}
		}
		cur, err := s.Repo.TeamSelections(ctx, tx, teamID)
		if err != nil {
			return err
		}
		have := map[string]Selection{}
		for _, sel := range cur {
			have[sel.EndpointID] = sel
		}
		var added, removed []TeamEndpoint
		for i, id := range ids {
			if _, ok := have[id]; ok {
				continue
			}
			te, err := s.Repo.TeamEndpoint(ctx, tx, teamID, id)
			if err != nil {
				return refused(fmt.Sprintf("endpointIds[%d]", i), "not a service endpoint", "endpoint "+id+" is not in the service catalog")
			}
			if te.IsTeam() {
				return refused(fmt.Sprintf("endpointIds[%d]", i), "not a service endpoint", "a team's own endpoints are always in its set")
			}
			if !admin && (te.RequiresTerms || te.SelectableBy == domain.SelectionServiceAdmin) {
				return errDenied{perm: authz.AdminModelEndpoints, reason: domain.ReasonNeedsServiceAdmin,
					msg: "endpoint " + te.Slug + " is selected for a team by a service admin"}
			}
			added = append(added, te)
		}
		for _, sel := range cur {
			if slices.Contains(ids, sel.EndpointID) {
				continue
			}
			if !admin && sel.SelectedBy == string(domain.SelectionServiceAdmin) {
				return errDenied{perm: authz.AdminModelEndpoints, reason: domain.ReasonNeedsServiceAdmin,
					msg: "a service admin selected " + sel.Slug + " for the team; only a service admin removes it"}
			}
			te, err := s.Repo.TeamEndpoint(ctx, tx, teamID, sel.EndpointID)
			if err != nil {
				return err
			}
			removed = append(removed, te)
		}
		for _, te := range added {
			if err := s.Repo.Select(ctx, tx, teamID, te.ID); err != nil {
				return err
			}
		}
		for _, te := range removed {
			if err := s.Repo.Deselect(ctx, tx, teamID, te.ID); err != nil {
				return err
			}
		}
		after, err := s.Repo.TeamSelections(ctx, tx, teamID)
		if err != nil {
			return err
		}
		out = SelectionsResult{TeamID: teamID, Selections: after}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamModelEndpoints", kindSelection, teamID, teamID,
			map[string]any{"endpointIds": SelectionsResult{Selections: cur}.EndpointIDs()}, map[string]any{"endpointIds": out.EndpointIDs()})); err != nil {
			return err
		}
		for _, te := range added {
			if err := s.Emit(ctx, tx, c, domain.SelectionEvent(true), teamID, kindEndpoint, te.ID, eventData(te.ModelEndpoint, map[string]any{"teamId": teamID})); err != nil {
				return err
			}
		}
		for _, te := range removed {
			if err := s.Emit(ctx, tx, c, domain.SelectionEvent(false), teamID, kindEndpoint, te.ID, eventData(te.ModelEndpoint, map[string]any{"teamId": teamID})); err != nil {
				return err
			}
		}
		return nil
	})
	return out, s.deny(ctx, c, "PutTeamModelEndpoints", teamID, err)
}

// CreateTeamEndpoint implements CreateTeamModelEndpoint. The team needs mayAddModelEndpoints (403
// team_endpoints_not_allowed); the provider is derived from the base URL host; inline prices need a
// service admin (only service admins write prices, database contract §7.4).
func (s *Service) CreateTeamEndpoint(ctx context.Context, c acting.Caller, teamID string, in NewEndpoint, idem *acting.Idem) (Created[Endpoint], error) {
	var res Created[Endpoint]
	const action = "CreateTeamModelEndpoint"
	if err := s.teamCheck(ctx, c, action, teamID, authz.ModelEndpointsWrite); err != nil {
		return res, err
	}
	e := in.Endpoint
	e.TeamID, e.Enabled, e.Builtin, e.RequiresTerms, e.CredentialMode = teamID, true, false, false, domain.CredentialPerTeam
	e.Provider = domain.ProviderOther
	defaults(&e)
	if err := e.Validate(); err != nil {
		return res, err
	}
	if err := domain.ValidateEndpointModels(modelsOf(in.Models), "models"); err != nil {
		return res, err
	}
	admin := isServiceAdmin(c)
	if err := pricesNeedAdmin(in.Models, admin); err != nil {
		return res, s.deny(ctx, c, action, teamID, err)
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[Endpoint]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		g, err := s.Repo.TeamGateway(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if !g.MayAdd && !admin {
			return errDenied{perm: authz.AdminModelEndpoints, reason: domain.ReasonTeamEndpointsNotAllowed,
				msg: "this team may not declare its own model endpoints; ask a service admin"}
		}
		providers, err := s.Repo.ListProviders(ctx, tx)
		if err != nil {
			return err
		}
		ps := make([]domain.InferenceProvider, 0, len(providers))
		for _, p := range providers {
			if p.Provider != domain.ProviderOther {
				ps = append(ps, p.InferenceProvider)
			}
		}
		e.Provider = domain.ProviderFor(ps, e)
		if err := e.Validate(); err != nil {
			return err
		}
		out, err := s.insert(ctx, tx, e, in.Models)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindEndpoint, out.ID, teamID, nil, auditEndpoint(out.ModelEndpoint))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EndpointLifecycleEvent(true), teamID, kindEndpoint, out.ID, eventData(out.ModelEndpoint, nil)); err != nil {
			return err
		}
		res.Value = out
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return res, s.deny(ctx, c, action, teamID, err)
}

// pricesNeedAdmin refuses inline prices from a caller who is not a service admin.
func pricesNeedAdmin(models []ModelInput, admin bool) error {
	if admin {
		return nil
	}
	for _, m := range models {
		if m.Price != nil {
			return errDenied{perm: authz.AdminPricing, reason: domain.ReasonNeedsServiceAdmin, msg: "route prices are set by service admins"}
		}
	}
	return nil
}

// ownEndpoint loads one of the team's own endpoints, locked; a service endpoint the team sees is a Conflict.
func (s *Service) ownEndpoint(ctx context.Context, tx port.Tx, teamID, id string) (Endpoint, error) {
	te, err := s.Repo.TeamEndpoint(ctx, tx, teamID, id)
	if err != nil {
		return Endpoint{}, err
	}
	if te.TeamID != teamID {
		return Endpoint{}, domain.Conflict("not_team_endpoint", "a service endpoint is changed by service admins", nil)
	}
	return s.Repo.GetEndpoint(ctx, tx, id, true)
}

// UpdateTeamEndpoint implements UpdateTeamModelEndpoint (the team's own endpoints; If-Match required).
func (s *Service) UpdateTeamEndpoint(ctx context.Context, c acting.Caller, teamID, id string, p Patch, expect domain.Version) (Endpoint, error) {
	if err := s.teamCheck(ctx, c, "UpdateTeamModelEndpoint", teamID, authz.ModelEndpointsWrite); err != nil {
		return Endpoint{}, err
	}
	// Level, provider, terms and the credential mode are not the team's to set.
	p.Provider, p.RequiresTerms, p.CredentialMode, p.ExternalSystem, p.UseProviderCost = nil, nil, nil, nil, nil
	var out Endpoint
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.ownEndpoint(ctx, tx, teamID, id)
		if err != nil {
			return err
		}
		if p.BaseURL != nil || p.Location != nil {
			providers, err := s.Repo.ListProviders(ctx, tx)
			if err != nil {
				return err
			}
			next, _ := p.apply(cur.ModelEndpoint)
			ps := make([]domain.InferenceProvider, 0, len(providers))
			for _, pr := range providers {
				if pr.Provider != domain.ProviderOther {
					ps = append(ps, pr.InferenceProvider)
				}
			}
			prov := domain.ProviderFor(ps, next)
			p.Provider = &prov
		}
		out, err = s.update(ctx, tx, c, "UpdateTeamModelEndpoint", cur, p, expect)
		return err
	})
	return out, err
}

// DeleteTeamEndpoint implements DeleteTeamModelEndpoint (the team's own endpoints; If-Match required).
func (s *Service) DeleteTeamEndpoint(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) error {
	if err := s.teamCheck(ctx, c, "DeleteTeamModelEndpoint", teamID, authz.ModelEndpointsWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.ownEndpoint(ctx, tx, teamID, id)
		if err != nil {
			return err
		}
		return s.delete(ctx, tx, c, "DeleteTeamModelEndpoint", cur, expect)
	})
}

// PutTeamModels implements PutTeamEndpointModels; prices may be set inline by a service admin.
func (s *Service) PutTeamModels(ctx context.Context, c acting.Caller, teamID, id string, models []ModelInput, expect *domain.Version) (ModelList, error) {
	const action = "PutTeamEndpointModels"
	if err := s.teamCheck(ctx, c, action, teamID, authz.ModelEndpointsWrite); err != nil {
		return ModelList{}, err
	}
	if err := pricesNeedAdmin(models, isServiceAdmin(c)); err != nil {
		return ModelList{}, s.deny(ctx, c, action, teamID, err)
	}
	var out ModelList
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.ownEndpoint(ctx, tx, teamID, id)
		if err != nil {
			return err
		}
		out, err = s.putModels(ctx, tx, c, action, cur, models, expect)
		return err
	})
	return out, err
}
