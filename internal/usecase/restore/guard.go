package restore

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// guardedSource memoizes the catalog reads of one restore, so a restore of many agents costs one lookup
// per distinct template, model, storage class and (prefetched) secret, not one per agent or field.
type guardedSource struct {
	src       agents.GuardSource
	templates map[string]tplResult
	models    map[string]bool
	classes   map[string]bool
	secrets   map[string]*agents.SecretInfo // nil value: known to be absent
}

type tplResult struct {
	t  agents.TemplateInfo
	ok bool
}

func newGuardedSource(src agents.GuardSource) *guardedSource {
	return &guardedSource{src: src, templates: map[string]tplResult{}, models: map[string]bool{},
		classes: map[string]bool{}, secrets: map[string]*agents.SecretInfo{}}
}

func (g *guardedSource) Template(ctx context.Context, tx port.Tx, name string) (agents.TemplateInfo, bool, error) {
	if r, ok := g.templates[name]; ok {
		return r.t, r.ok, nil
	}
	t, ok, err := g.src.Template(ctx, tx, name)
	if err != nil {
		return agents.TemplateInfo{}, false, err
	}
	g.templates[name] = tplResult{t, ok}
	return t, ok, nil
}

func (g *guardedSource) ModelEnabled(ctx context.Context, tx port.Tx, teamID, model string) (bool, error) {
	if ok, seen := g.models[model]; seen {
		return ok, nil
	}
	ok, err := g.src.ModelEnabled(ctx, tx, teamID, model)
	if err != nil {
		return false, err
	}
	g.models[model] = ok
	return ok, nil
}

func (g *guardedSource) StorageClassEnabled(ctx context.Context, tx port.Tx, name string) (bool, error) {
	if ok, seen := g.classes[name]; seen {
		return ok, nil
	}
	ok, err := g.src.StorageClassEnabled(ctx, tx, name)
	if err != nil {
		return false, err
	}
	g.classes[name] = ok
	return ok, nil
}

// Secrets reads only the ids not seen before, in one call.
func (g *guardedSource) Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]agents.SecretInfo, error) {
	var missing []string
	for _, id := range ids {
		if _, seen := g.secrets[id]; !seen && !slices.Contains(missing, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		found, err := g.src.Secrets(ctx, tx, teamID, missing)
		if err != nil {
			return nil, err
		}
		for _, id := range missing {
			if info, ok := found[id]; ok {
				g.secrets[id] = &info
			} else {
				g.secrets[id] = nil
			}
		}
	}
	out := map[string]agents.SecretInfo{}
	for _, id := range ids {
		if info := g.secrets[id]; info != nil {
			out[id] = *info
		}
	}
	return out, nil
}

// guardSteps applies the agent guardrails of agents.Guard to a restore plan. A step the agents use case
// would refuse is not written: it becomes a skip whose reason names the guard. Only the fields a step
// changes are checked, as UpdateAgent does, so an agent whose template was retired can still have its
// persona restored. Reads come from the transaction the plan was made in.
func guardSteps(ctx context.Context, tx port.Tx, src agents.GuardSource, teamID string, steps []domain.RestoreStep) ([]domain.RestoreStep, error) {
	if src == nil {
		return nil, errors.New("restore: no guard source configured")
	}
	cache := newGuardedSource(src)
	g := agents.Guard{Src: cache}
	// One read for every secret a binding step names.
	var secretIDs []string
	for _, st := range steps {
		if id := bindingSecret(st); id != "" {
			secretIDs = append(secretIDs, id)
		}
	}
	if len(secretIDs) > 0 {
		if _, err := cache.Secrets(ctx, tx, teamID, secretIDs); err != nil {
			return nil, err
		}
	}
	out := make([]domain.RestoreStep, 0, len(steps))
	for _, st := range steps {
		if st.Change.Action != domain.RestoreSkip {
			why, err := refusal(ctx, tx, g, teamID, st)
			if err != nil {
				return nil, err
			}
			if why != "" {
				st.Change.Action, st.Change.Reason, st.Change.Fields, st.Row = domain.RestoreSkip, why, nil, nil
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// bindingSecret returns the secret a create or update step of agent_secret_bindings binds.
func bindingSecret(st domain.RestoreStep) string {
	if st.Change.ResourceKind != "agent_secret_bindings" || st.Row == nil || st.Change.Action == domain.RestoreDelete {
		return ""
	}
	return str(st.Row, "secret_id")
}

func str(r domain.Row, k string) string { s, _ := r[k].(string); return s }

// refusal returns why the agents guardrails refuse step, or "" when it may be applied.
func refusal(ctx context.Context, tx port.Tx, g agents.Guard, teamID string, st domain.RestoreStep) (string, error) {
	var err error
	switch st.Change.ResourceKind {
	case "agents":
		if st.Change.Action != domain.RestoreUpdate || st.Row == nil {
			return "", nil
		}
		changed := func(f string) bool {
			return slices.ContainsFunc(st.Change.Fields, func(c domain.FieldChange) bool { return c.Field == f })
		}
		if changed("template") || changed("image") {
			_, err = g.CheckTemplate(ctx, tx, str(st.Row, "template"), domain.ImageRef(str(st.Row, "image")))
		}
		if err == nil && changed("model_id") {
			err = g.CheckModel(ctx, tx, teamID, str(st.Row, "model_id"))
		}
		if err == nil && changed("storage_class") {
			err = g.CheckStorageClass(ctx, tx, str(st.Row, "storage_class"))
		}
	case "agent_secret_bindings":
		if id := bindingSecret(st); id != "" {
			agent, _, _ := strings.Cut(strings.TrimPrefix(st.Change.ResourceID, teamID+"/"), "/")
			err = g.CheckBindings(ctx, tx, teamID, agent, []domain.SecretBinding{{SecretID: id}})
		}
	}
	var de *domain.Error
	if err == nil || !errors.As(err, &de) || domain.KindOf(err) != domain.KindValidation {
		return "", err
	}
	return "refused by the " + guardName(de.Fields) + ": " + reasons(de.Fields), nil
}

// guardName names the guard that refused: the API field of the first field error.
func guardName(fe []domain.FieldError) string {
	if len(fe) == 0 {
		return "agent guard"
	}
	switch f := fe[0].Field; {
	case f == "template":
		return "template guard"
	case f == "image":
		return "template guard (digest)"
	case f == "modelId":
		return "model guard"
	case strings.HasPrefix(f, "volume"):
		return "storage class guard"
	case strings.HasPrefix(f, "secretBindings"):
		return "binding guard"
	default:
		return "agent guard"
	}
}

func reasons(fe []domain.FieldError) string {
	parts := make([]string, 0, len(fe))
	for _, f := range fe {
		parts = append(parts, f.Reason)
	}
	return strings.Join(parts, "; ")
}
