package modelendpoints

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ListProviders implements ListInferenceProviders.
func (s *Service) ListProviders(ctx context.Context, c acting.Caller) ([]Provider, error) {
	if err := s.serviceCheck(ctx, c, "ListInferenceProviders", authz.AdminModelEndpoints); err != nil {
		return nil, err
	}
	var out []Provider
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.ListProviders(ctx, tx)
		return err
	})
	return out, err
}

// ProviderPatch is an UpdateInferenceProvider request.
type ProviderPatch struct {
	Enabled       *bool
	TeamSelection *domain.TeamSelection
	Reason        *string
	// FallbackModel and SelectForAllTeams apply when Bedrock is turned off.
	FallbackModel     *string
	SelectForAllTeams bool
}

func (p ProviderPatch) validate(provider string) error {
	var fe []domain.FieldError
	if p.TeamSelection != nil && *p.TeamSelection != domain.SelectionTeamAdmin && *p.TeamSelection != domain.SelectionServiceAdmin {
		fe = append(fe, domain.FieldError{Field: "teamSelection", Reason: "must be team_admin or service_admin"})
	}
	if p.Reason != nil && len(*p.Reason) > 500 {
		fe = append(fe, domain.FieldError{Field: "reason", Reason: "at most 500 characters"})
	}
	if provider != domain.ProviderBedrock && (p.FallbackModel != nil || p.SelectForAllTeams) {
		fe = append(fe, domain.FieldError{Field: "fallbackModel", Reason: "only when Bedrock is turned off"})
	}
	if p.FallbackModel != nil {
		if _, _, ok := domain.ParseRouteModel(*p.FallbackModel); !ok || len(*p.FallbackModel) > 240 {
			fe = append(fe, domain.FieldError{Field: "fallbackModel", Reason: "must be <route>/<model>"})
		}
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

func auditProvider(p domain.InferenceProvider) map[string]any {
	return map[string]any{"provider": p.Provider, "enabled": p.Enabled, "teamSelection": p.TeamSelection,
		"disabledReason": p.DisabledReason, "fallbackModel": p.FallbackModel}
}

// UpdateProvider implements UpdateInferenceProvider. dryRun returns the impact counts of turning the
// provider off and writes nothing. Turning Bedrock off needs a fallback route model: it becomes the service
// default (and, with SelectForAllTeams, its endpoint is selected for every team first), and the database
// refuses the switch otherwise (no_fallback_model).
func (s *Service) UpdateProvider(ctx context.Context, c acting.Caller, provider string, p ProviderPatch, dryRun bool, expect domain.Version) (Provider, error) {
	if err := s.serviceCheck(ctx, c, "UpdateInferenceProvider", authz.AdminInferenceProviders); err != nil {
		return Provider{}, err
	}
	if err := p.validate(provider); err != nil {
		return Provider{}, err
	}
	run := s.Tx.InTx
	if dryRun {
		run = s.Tx.ReadTx
	}
	var out Provider
	err := run(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetProvider(ctx, tx, provider, !dryRun)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(domain.Version(cur.Version), expect); err != nil {
			return err
		}
		next := cur.InferenceProvider
		if p.Enabled != nil {
			next.Enabled = *p.Enabled
		}
		if p.TeamSelection != nil {
			next.TeamSelection = *p.TeamSelection
		}
		if p.Reason != nil {
			next.DisabledReason = *p.Reason
		}
		if next.Enabled {
			next.DisabledReason = ""
		}
		turningOff := cur.Enabled && !next.Enabled
		fallback := ""
		if turningOff && provider == domain.ProviderBedrock {
			if p.FallbackModel == nil {
				return refused("fallbackModel", domain.ReasonNoFallbackModel, "turning Bedrock off needs fallbackModel, a <route>/<model> on an enabled service endpoint")
			}
			fallback = *p.FallbackModel
			next.FallbackModel = fallback
		}
		if dryRun {
			im, err := s.Repo.ProviderImpact(ctx, tx, provider, fallback)
			if err != nil {
				return err
			}
			out = cur
			out.Impact = &im
			return nil
		}
		if fallback != "" {
			if err := s.applyFallback(ctx, tx, fallback, p.SelectForAllTeams); err != nil {
				return err
			}
		}
		if err := s.Repo.UpdateProvider(ctx, tx, next, expect); err != nil {
			return err
		}
		if out, err = s.Repo.GetProvider(ctx, tx, provider, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateInferenceProvider", kindProvider, provider, "", auditProvider(cur.InferenceProvider), auditProvider(out.InferenceProvider))); err != nil {
			return err
		}
		data := map[string]any{"provider": provider, "enabled": out.Enabled, "teamSelection": string(out.TeamSelection)}
		if out.FallbackModel != "" {
			data["fallbackModel"] = out.FallbackModel
		}
		return s.Emit(ctx, tx, c, domain.ProviderSwitchEvent(out.Enabled), "", kindProvider, provider, data)
	})
	return out, err
}

func (s *Service) applyFallback(ctx context.Context, tx port.Tx, fallback string, selectAll bool) error {
	route, model, _ := domain.ParseRouteModel(fallback)
	e, err := s.Repo.ServiceEndpointBySlug(ctx, tx, route)
	if err != nil {
		return refused("fallbackModel", domain.ReasonNoFallbackModel, "no service endpoint has route "+route)
	}
	listed := false
	for _, m := range e.Models {
		listed = listed || (m.ModelName == model && m.Enabled)
	}
	if !e.Enabled || !listed {
		return refused("fallbackModel", domain.ReasonNoFallbackModel, "the fallback must be an enabled model on an enabled service endpoint")
	}
	if selectAll {
		if _, err := s.Repo.SelectForAllTeams(ctx, tx, e.ID); err != nil {
			return err
		}
	}
	return s.Repo.SetServiceDefaultModel(ctx, tx, fallback)
}

// TeamGatewayPatch is a PutTeamGatewaySettings request; nil members are kept.
type TeamGatewayPatch struct {
	Deployment *domain.GatewayDeployment
	MayAdd     *bool
}

// PutTeamGateway implements PutTeamGatewaySettings (teams:set-guardrails). If-Match, when sent, is the
// team's version.
func (s *Service) PutTeamGateway(ctx context.Context, c acting.Caller, teamID string, p TeamGatewayPatch, expect *domain.Version) (TeamGateway, error) {
	if err := s.teamCheck(ctx, c, "PutTeamGatewaySettings", teamID, authz.TeamsSetGuardrails); err != nil {
		return TeamGateway{}, err
	}
	if p.Deployment != nil && *p.Deployment != domain.GatewayShared && *p.Deployment != domain.GatewayPerNamespace {
		return TeamGateway{}, refused("deployment", "must be shared or per_namespace", "")
	}
	var out TeamGateway
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.TeamGateway(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		d, may := cur.Deployment, cur.MayAdd
		if p.Deployment != nil {
			d = *p.Deployment
		}
		if p.MayAdd != nil {
			may = *p.MayAdd
		}
		if err := s.Repo.PutTeamGateway(ctx, tx, teamID, d, may, cur.Version); err != nil {
			return err
		}
		if out, err = s.Repo.TeamGateway(ctx, tx, teamID, false); err != nil {
			return err
		}
		before := map[string]any{"gatewayDeployment": cur.Deployment, "mayAddModelEndpoints": cur.MayAdd}
		after := map[string]any{"gatewayDeployment": out.Deployment, "mayAddModelEndpoints": out.MayAdd}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamGatewaySettings", kindTeam, teamID, teamID, before, after)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamUpdated, teamID, kindTeam, teamID, map[string]any{"teamId": teamID,
			"changed": []string{"gatewayDeployment", "mayAddModelEndpoints"}}); err != nil {
			return err
		}
		out = s.gatewayView(out)
		return nil
	})
	return out, err
}

// TeamGatewayPort is the port of a team gateway's listener (gateway contract §12.1).
const TeamGatewayPort = 8443

// TeamGatewayURL is the AFS_GATEWAY_URL of a team on its own gateway.
func TeamGatewayURL(namespace string) string {
	return "https://afs-gateway." + namespace + ".svc:8443"
}

// gatewayView derives the URL and state: shared until the reconciler has rendered the team gateway.
func (s *Service) gatewayView(t TeamGateway) TeamGateway {
	if t.Deployment == domain.GatewayShared {
		t.GatewayURL, t.GatewayState = s.Config.SharedGatewayURL, GatewayStateShared
		return t
	}
	t.GatewayURL = TeamGatewayURL(t.Namespace)
	switch t.GatewayPhase {
	case "Ready":
		t.GatewayState = GatewayStateReady
	case "Degraded", "Failed":
		t.GatewayState = GatewayStateDegraded
	default:
		t.GatewayState = GatewayStateProvisioning
	}
	return t
}
