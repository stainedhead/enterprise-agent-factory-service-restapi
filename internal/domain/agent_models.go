package domain

import (
	"slices"
	"strconv"
	"strings"
)

// Model access at the service, team and agent level (API contract §4.4 Models, root PRD MOD-1 to MOD-8).
// Each level is either all (every model the level above allows, now and later) or a selection.

// ModelAccess is all or selected.
type ModelAccess string

// ModelAccess values.
const (
	ModelAccessAll      ModelAccess = "all"
	ModelAccessSelected ModelAccess = "selected"
)

// Valid reports whether m is all or selected.
func (m ModelAccess) Valid() bool { return m == ModelAccessAll || m == ModelAccessSelected }

// MaxSelectedModels is the most models a selection may name (agents.models, team_models).
const MaxSelectedModels = 50

// Reasons for model validation failures and for the gateway's refusals (MOD-5).
const (
	ReasonModelNotEnabledForService = "model_not_enabled_for_service"
	ReasonModelNotEnabledForTeam    = "model_not_enabled_for_team"
	ReasonModelNotEnabledForAgent   = "model_not_enabled_for_agent"
	ReasonModelNotPriced            = "model_not_priced"
	ReasonModelsSelectionEmpty      = "models_selection_empty"
	ReasonDefaultModelNotAllowed    = "default_model_not_allowed"
	ReasonModelIDsNotAllowedForAll  = "model_ids_not_allowed_for_all"
	ReasonConflictingFields         = "conflicting_fields"
	ReasonNoModelAvailable          = "no_model_available"
	ReasonServiceModelAccessAll     = "service_model_access_all"
)

// AgentModels is an agent's or pool's model access (MOD-3). Effective is computed at read time as
// service ∩ team ∩ agent ∩ callable and is never stored; ModelIDs keeps stored ids that dropped out
// (MOD-6).
type AgentModels struct {
	Access         ModelAccess
	ModelIDs       []string
	DefaultModelID string
	Effective      []string
}

// ModelsInput is the models object of a CreateAgent, UpdateAgent, CreatePool or UpdatePool body. A nil
// member is omitted: on create it takes its default, on update the stored value is kept.
type ModelsInput struct {
	Access         *ModelAccess
	ModelIDs       *[]string
	DefaultModelID *string
	// Routes holds the v1.5.0 route members (routeAccess, routes, defaultRoute); nil when none is sent.
	Routes *RoutesInput
}

// RoutesInput is the route part of a models object (API contract v1.5.0 §4.13): routeAccess, routes and
// defaultRoute. A nil member is omitted; DefaultRouteNull marks an explicit null defaultRoute.
type RoutesInput struct {
	Access           *string
	Routes           *[]string
	DefaultRoute     *string
	DefaultRouteNull bool
}

// Present reports whether any member is set.
func (m *ModelsInput) Present() bool {
	return m != nil && (m.Access != nil || m.ModelIDs != nil || m.DefaultModelID != nil || m.Routes != nil)
}

// routesNotYet refuses the route members until route admission is implemented (release 1.2.0, the
// implementation change after the v1.5.0 contract change): never silently ignored.
func routesNotYet(in *ModelsInput) []FieldError {
	if in == nil || in.Routes == nil {
		return nil
	}
	return []FieldError{{Field: "models.routeAccess", Reason: ReasonNotImplemented}}
}

// ModelSets are the sets the validation checks a request against, read in the request's transaction.
type ModelSets struct {
	// Service holds the ids enabled for the service (every catalog model while service access is all).
	Service []string
	// Team holds the ids the team may use: Service when the team's access is all, else Service ∩ the
	// team's selection.
	Team []string
	// Callable holds the ids with a price in force; a default is picked from it.
	Callable []string
	// SettingDefault is models.default_model_id; empty when unset.
	SettingDefault string
}

func (s ModelSets) inService(id string) bool { return slices.Contains(s.Service, id) }
func (s ModelSets) inTeam(id string) bool    { return slices.Contains(s.Team, id) }

// firstCallable returns the team's first callable model by id, "" when there is none.
func (s ModelSets) firstCallable() string {
	best := ""
	for _, id := range s.Team {
		if slices.Contains(s.Callable, id) && (best == "" || id < best) {
			best = id
		}
	}
	return best
}

// checkSelection checks each listed id against the service and team sets.
func checkSelection(ids []string, sets ModelSets, field string) []FieldError {
	var fe []FieldError
	seen := map[string]bool{}
	for i, id := range ids {
		f := field + "[" + strconv.Itoa(i) + "]"
		switch {
		case strings.TrimSpace(id) == "":
			fe = append(fe, FieldError{Field: f, Reason: "must not be empty"})
		case seen[id]:
			fe = append(fe, FieldError{Field: f, Reason: "is listed twice"})
		case !sets.inService(id):
			fe = append(fe, FieldError{Field: f, Reason: ReasonModelNotEnabledForService})
		case !sets.inTeam(id):
			fe = append(fe, FieldError{Field: f, Reason: ReasonModelNotEnabledForTeam})
		}
		seen[id] = true
	}
	return fe
}

// ResolveNewModels builds the models of a new agent or pool (CD-31). legacyModelID is the deprecated
// modelId alias: alone it creates {access: all, defaultModelId: modelId}; with models.defaultModelId and a
// different value it is a conflict. field is the prefix of the reported fields ("models").
func ResolveNewModels(in *ModelsInput, legacyModelID string, sets ModelSets) (AgentModels, error) {
	fe := routesNotYet(in)
	out := AgentModels{Access: ModelAccessAll, ModelIDs: []string{}}
	def := legacyModelID
	if in != nil {
		if in.Access != nil {
			if !in.Access.Valid() {
				fe = append(fe, FieldError{Field: "models.access", Reason: "must be all or selected"})
			} else {
				out.Access = *in.Access
			}
		}
		if in.ModelIDs != nil {
			out.ModelIDs = slices.Clone(*in.ModelIDs)
		}
		if in.DefaultModelID != nil {
			if legacyModelID != "" && legacyModelID != *in.DefaultModelID {
				fe = append(fe, FieldError{Field: "modelId", Reason: ReasonConflictingFields})
			}
			def = *in.DefaultModelID
		}
	}
	if len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	if in != nil && in.Access == nil && in.ModelIDs != nil && len(*in.ModelIDs) > 0 {
		// An explicit list without an access is read as a selection.
		out.Access = ModelAccessSelected
	}
	fe = validateAccess(&out, sets)
	if len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	if def == "" {
		switch {
		case sets.SettingDefault != "" && sets.inTeam(sets.SettingDefault) && out.allows(sets.SettingDefault):
			def = sets.SettingDefault
		case out.Access == ModelAccessSelected:
			def = firstOf(out.ModelIDs)
		default:
			def = sets.firstCallable()
		}
	}
	if def == "" {
		return AgentModels{}, Validation(FieldError{Field: "models.defaultModelId", Reason: "required"})
	}
	out.DefaultModelID = def
	if fe := checkDefault(out, sets); len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	return out, nil
}

func firstOf(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return slices.Min(ids)
}

// allows reports whether the agent's own selection admits the model (always, for all).
func (m AgentModels) allows(id string) bool {
	return m.Access == ModelAccessAll || slices.Contains(m.ModelIDs, id)
}

// validateAccess checks the access, the selection and its size against the sets.
func validateAccess(m *AgentModels, sets ModelSets) []FieldError {
	var fe []FieldError
	switch m.Access {
	case ModelAccessAll:
		if len(m.ModelIDs) > 0 {
			fe = append(fe, FieldError{Field: "models.modelIds", Reason: ReasonModelIDsNotAllowedForAll})
		}
	case ModelAccessSelected:
		switch {
		case len(m.ModelIDs) == 0:
			fe = append(fe, FieldError{Field: "models.modelIds", Reason: ReasonModelsSelectionEmpty})
		case len(m.ModelIDs) > MaxSelectedModels:
			fe = append(fe, FieldError{Field: "models.modelIds", Reason: "at most " + strconv.Itoa(MaxSelectedModels) + " models"})
		default:
			fe = append(fe, checkSelection(m.ModelIDs, sets, "models.modelIds")...)
		}
	default:
		fe = append(fe, FieldError{Field: "models.access", Reason: "must be all or selected"})
	}
	return fe
}

// checkDefault requires the default to be in the agent's own selection and in the team's set.
func checkDefault(m AgentModels, sets ModelSets) []FieldError {
	if !sets.inTeam(m.DefaultModelID) || !m.allows(m.DefaultModelID) {
		return []FieldError{{Field: "models.defaultModelId", Reason: ReasonDefaultModelNotAllowed}}
	}
	return nil
}

// ApplyModels returns cur with the members of in that are set (CD-31: members sent replace the stored
// ones, omitted members are kept). legacyModelID alone sets only the default. Switching to all clears the
// selection. The result is validated against sets as a whole.
func (cur AgentModels) ApplyModels(in *ModelsInput, legacyModelID string, sets ModelSets) (AgentModels, error) {
	out := AgentModels{Access: cur.Access, ModelIDs: slices.Clone(cur.ModelIDs), DefaultModelID: cur.DefaultModelID}
	fe := routesNotYet(in)
	if in != nil {
		if in.Access != nil {
			if !in.Access.Valid() {
				fe = append(fe, FieldError{Field: "models.access", Reason: "must be all or selected"})
			}
			out.Access = *in.Access
		}
		if in.ModelIDs != nil {
			out.ModelIDs = slices.Clone(*in.ModelIDs)
		}
		if in.DefaultModelID != nil {
			if legacyModelID != "" && legacyModelID != *in.DefaultModelID {
				fe = append(fe, FieldError{Field: "modelId", Reason: ReasonConflictingFields})
			}
			out.DefaultModelID = *in.DefaultModelID
		}
	}
	if legacyModelID != "" && (in == nil || in.DefaultModelID == nil) {
		out.DefaultModelID = legacyModelID
	}
	if len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	if in != nil && in.Access != nil && *in.Access == ModelAccessAll && in.ModelIDs == nil {
		out.ModelIDs = []string{}
	}
	if out.Access == ModelAccessAll && in != nil && in.ModelIDs != nil && len(*in.ModelIDs) > 0 {
		return AgentModels{}, Validation(FieldError{Field: "models.modelIds", Reason: ReasonModelIDsNotAllowedForAll})
	}
	if out.Access == ModelAccessAll {
		out.ModelIDs = []string{}
	}
	if fe := validateAccess(&out, sets); len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	if fe := checkDefault(out, sets); len(fe) > 0 {
		return AgentModels{}, Validation(fe...)
	}
	return out, nil
}

// Equal reports whether two models objects store the same thing (Effective is not stored).
func (m AgentModels) Equal(o AgentModels) bool {
	a, b := slices.Clone(m.ModelIDs), slices.Clone(o.ModelIDs)
	slices.Sort(a)
	slices.Sort(b)
	return m.Access == o.Access && m.DefaultModelID == o.DefaultModelID && slices.Equal(a, b)
}

// ComputeEffective returns the model ids the subject may call now: the team set ∩ the subject's own
// selection ∩ callable, default first, then by id (MOD-4 order).
func (m AgentModels) ComputeEffective(team, callable []string) []string {
	out := []string{}
	for _, id := range team {
		if m.allows(id) && slices.Contains(callable, id) {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		switch {
		case a == m.DefaultModelID && b != m.DefaultModelID:
			return -1
		case b == m.DefaultModelID && a != m.DefaultModelID:
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}

// ModelUnavailable reports whether the default left the effective set, and whether the set is empty
// (the agent is Degraded then, MOD-6).
func (m AgentModels) ModelUnavailable(effective []string) (defaultGone, empty bool) {
	return !slices.Contains(effective, m.DefaultModelID), len(effective) == 0
}

// TeamModelAccess is a team's model access: all, or the explicit list (MOD-2, ListTeamModels).
type TeamModelAccess struct {
	Access   ModelAccess
	ModelIDs []string
}

// ValidateTeamModelAccess checks a PutTeamModels body: all takes no ids, selected takes 1 to 50 that
// are enabled for the service. A body with only modelIds is read as selected (CD-30).
func ValidateTeamModelAccess(access ModelAccess, ids []string, serviceEnabled []string) (TeamModelAccess, error) {
	if access == "" && ids != nil {
		access = ModelAccessSelected
	}
	if access == "" {
		return TeamModelAccess{}, Validation(FieldError{Field: "access", Reason: "is required"})
	}
	var fe []FieldError
	switch access {
	case ModelAccessAll:
		if len(ids) > 0 {
			fe = append(fe, FieldError{Field: "modelIds", Reason: ReasonModelIDsNotAllowedForAll})
		}
		ids = []string{}
	case ModelAccessSelected:
		switch {
		case len(ids) == 0:
			fe = append(fe, FieldError{Field: "modelIds", Reason: ReasonModelsSelectionEmpty})
		case len(ids) > MaxSelectedModels:
			fe = append(fe, FieldError{Field: "modelIds", Reason: "at most " + strconv.Itoa(MaxSelectedModels) + " models"})
		default:
			seen := map[string]bool{}
			for i, id := range ids {
				f := "modelIds[" + strconv.Itoa(i) + "]"
				switch {
				case seen[id]:
					fe = append(fe, FieldError{Field: f, Reason: "is listed twice"})
				case !slices.Contains(serviceEnabled, id):
					fe = append(fe, FieldError{Field: f, Reason: ReasonModelNotEnabledForService})
				}
				seen[id] = true
			}
		}
	default:
		fe = append(fe, FieldError{Field: "access", Reason: "must be all or selected"})
	}
	if len(fe) > 0 {
		return TeamModelAccess{}, Validation(fe...)
	}
	return TeamModelAccess{Access: access, ModelIDs: ids}, nil
}

// Audit is the audit-record form of the models (the stored selection and the default, never the
// effective list, which is computed).
func (m AgentModels) Audit() map[string]any {
	ids := m.ModelIDs
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{"access": string(m.Access), "modelIds": ids, "defaultModelId": m.DefaultModelID}
}
