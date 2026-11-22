package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Swarm template limits (database 000012 swarm_templates; API contract §4.10a and §5).
const (
	// MinTemplateTTLHours and MaxTemplateTTLHours bound a remediation team's lifetime.
	MinTemplateTTLHours = 1
	MaxTemplateTTLHours = 8760
	// MaxTemplatePools bounds the pools of one template (a swarm is small; the cap keeps the spec sane).
	MaxTemplatePools = 50
	// MaxTemplateStages bounds the stages of one template; stage positions are 1 to MaxStagePosition.
	MaxTemplateStages = MaxStagePosition
	// MaxTemplateConfigValues and MaxTemplateSecrets bound the spec's lists.
	MaxTemplateConfigValues = 500
	MaxTemplateSecrets      = 100
	// MaxTemplateDescription is the database check on swarm_templates.description.
	MaxTemplateDescription = 1000
)

// SwarmTemplate is the recipe for a swarm (an ephemeral team): its stages, pools, handoffs,
// secrets and configuration values, the lifetime budget cap, the time to live and the teardown rule.
// A CVE remediation work order instantiates one into a remediation team (RC-12). The spec is held as
// the JSON document the database stores; ValidateSwarmSpec checks the part the instantiation reads.
type SwarmTemplate struct {
	ID           string
	Name         Name
	Description  string
	Kind         WorkOrderKind
	Spec         map[string]any
	Budget       Money
	TTLHours     int
	TeardownRule TeardownRule
	Enabled      bool
	Version      Version
	CreatedAt    time.Time
	CreatedBy    string
	UpdatedAt    time.Time
}

// NewSwarmTemplateParams is a CreateSwarmTemplate body. A nil Enabled creates the template enabled.
type NewSwarmTemplateParams struct {
	Name         string
	Description  string
	Kind         string
	Spec         map[string]any
	BudgetUSD    string
	TTLHours     int
	TeardownRule string
	Enabled      *bool
}

func validateTemplateTTL(h int) []FieldError {
	if h < MinTemplateTTLHours || h > MaxTemplateTTLHours {
		return []FieldError{{Field: "ttlHours", Reason: "must be " + itoa(MinTemplateTTLHours) + " to " + itoa(MaxTemplateTTLHours)}}
	}
	return nil
}

func validateTemplateBudget(field, s string) (Money, []FieldError) {
	m, err := ParseMoney(field, s)
	if err != nil {
		return Money{}, fieldErrorsOf(err)
	}
	if m.Micros() <= 0 {
		return Money{}, []FieldError{{Field: field, Reason: "must be greater than zero"}}
	}
	return m, nil
}

func validateTemplateRule(r string) []FieldError {
	if !TeardownRule(r).Valid() {
		return []FieldError{{Field: "teardownRule", Reason: "must be at_expiry, at_cap_or_expiry or at_work_complete"}}
	}
	return nil
}

func validateTemplateDescription(d string) []FieldError {
	if utf8.RuneCountInString(d) > MaxTemplateDescription {
		return []FieldError{{Field: "description", Reason: "must be at most " + itoa(MaxTemplateDescription) + " characters"}}
	}
	return nil
}

// NewSwarmTemplate validates p and returns the template at version 1; the id is set by the caller.
func NewSwarmTemplate(id string, p NewSwarmTemplateParams, now time.Time) (SwarmTemplate, error) {
	var fe []FieldError
	name, err := ParseName("name", p.Name)
	fe = append(fe, fieldErrorsOf(err)...)
	kind := WorkOrderKind(p.Kind)
	if !kind.Valid() {
		fe = append(fe, FieldError{Field: "kind", Reason: "must be cve_remediation or general"})
	}
	fe = append(fe, validateTemplateDescription(p.Description)...)
	fe = append(fe, ValidateSwarmSpec("spec", p.Spec)...)
	budget, bfe := validateTemplateBudget("budgetUsd", p.BudgetUSD)
	fe = append(fe, bfe...)
	fe = append(fe, validateTemplateTTL(p.TTLHours)...)
	fe = append(fe, validateTemplateRule(p.TeardownRule)...)
	if len(fe) > 0 {
		return SwarmTemplate{}, Validation(fe...)
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	return SwarmTemplate{ID: id, Name: name, Description: strings.TrimSpace(p.Description), Kind: kind, Spec: p.Spec,
		Budget: budget, TTLHours: p.TTLHours, TeardownRule: TeardownRule(p.TeardownRule), Enabled: enabled,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// SwarmTemplatePatch is an UpdateSwarmTemplate body: a nil field is left unchanged. The name and the
// kind never change.
type SwarmTemplatePatch struct {
	Description  *string
	Spec         map[string]any
	BudgetUSD    *string
	TTLHours     *int
	TeardownRule *string
}

// Empty reports whether the patch changes nothing.
func (p SwarmTemplatePatch) Empty() bool {
	return p.Description == nil && p.Spec == nil && p.BudgetUSD == nil && p.TTLHours == nil && p.TeardownRule == nil
}

// Apply returns the template with the patch applied. Changing a template touches no existing team or
// work order: only a work order instantiated later reads it.
func (t SwarmTemplate) Apply(p SwarmTemplatePatch, now time.Time) (SwarmTemplate, error) {
	var fe []FieldError
	if p.Description != nil {
		fe = append(fe, validateTemplateDescription(*p.Description)...)
		t.Description = strings.TrimSpace(*p.Description)
	}
	if p.Spec != nil {
		fe = append(fe, ValidateSwarmSpec("spec", p.Spec)...)
		t.Spec = p.Spec
	}
	if p.BudgetUSD != nil {
		m, bfe := validateTemplateBudget("budgetUsd", *p.BudgetUSD)
		fe = append(fe, bfe...)
		t.Budget = m
	}
	if p.TTLHours != nil {
		fe = append(fe, validateTemplateTTL(*p.TTLHours)...)
		t.TTLHours = *p.TTLHours
	}
	if p.TeardownRule != nil {
		fe = append(fe, validateTemplateRule(*p.TeardownRule)...)
		t.TeardownRule = TeardownRule(*p.TeardownRule)
	}
	if len(fe) > 0 {
		return t, Validation(fe...)
	}
	t.UpdatedAt = now.UTC()
	return t, nil
}

// WithEnabled returns the template switched on or off.
func (t SwarmTemplate) WithEnabled(on bool, now time.Time) SwarmTemplate {
	t.Enabled, t.UpdatedAt = on, now.UTC()
	return t
}

// Audit is the audit view of the template.
func (t SwarmTemplate) Audit() map[string]any {
	return map[string]any{"id": t.ID, "name": string(t.Name), "description": t.Description, "kind": string(t.Kind),
		"budgetUsd": t.Budget.String(), "ttlHours": t.TTLHours, "teardownRule": string(t.TeardownRule),
		"enabled": t.Enabled, "pools": len(listOf(t.Spec, "pools")), "stages": len(listOf(t.Spec, "stages"))}
}

// listOf returns spec[key] as a list, nil when it is absent or not a list.
func listOf(spec map[string]any, key string) []any {
	l, _ := spec[key].([]any)
	return l
}

// ValidateSwarmSpec checks the part of a swarm template spec that afs.instantiate_work_order_team
// reads (database contract §4, 000016): stages, pools with their stage, model, image and sizes,
// handoffs and configuration values naming pools the template defines, tags and the secret names.
// Keys it does not know pass through: the instantiation ignores them. field prefixes every error
// field (for example "spec").
func ValidateSwarmSpec(field string, spec map[string]any) []FieldError {
	var fe []FieldError
	bad := func(path, reason string) { fe = append(fe, FieldError{Field: field + path, Reason: reason}) }
	if spec == nil {
		return []FieldError{{Field: field, Reason: "is required"}}
	}
	for _, k := range []string{"stages", "pools", "handoffs", "secrets", "configValues"} {
		if v, ok := spec[k]; ok {
			if _, isList := v.([]any); !isList && v != nil {
				bad("."+k, "must be a list")
			}
		}
	}
	for _, k := range []string{"costCenter", "ownerEmail"} {
		if v, ok := spec[k]; ok {
			if s, isStr := v.(string); !isStr || utf8.RuneCountInString(s) > 254 {
				bad("."+k, "must be a string of at most 254 characters")
			}
		}
	}
	if v, ok := spec["tags"]; ok && v != nil {
		tags, isMap := v.(map[string]any)
		if !isMap {
			bad(".tags", "must be an object of strings")
		}
		for k, tv := range tags {
			if s, isStr := tv.(string); !isStr || k == "" || len(k) > 128 || len(s) > 256 {
				bad(".tags."+k, "keys and values must be non-empty strings (key at most 128, value at most 256 characters)")
			}
		}
	}

	stages := map[string]bool{}
	stageList := listOf(spec, "stages")
	if len(stageList) > MaxTemplateStages {
		bad(".stages", "has at most "+itoa(MaxTemplateStages)+" stages")
	}
	for i, raw := range stageList {
		at := fmt.Sprintf(".stages[%d]", i)
		st, ok := raw.(map[string]any)
		if !ok {
			bad(at, "must be an object")
			continue
		}
		name, nerr := ParseName(at+".name", asString(st["name"]))
		if nerr != nil {
			fe = append(fe, prefixed(field, fieldErrorsOf(nerr))...)
		} else if stages[string(name)] {
			bad(at+".name", "names a stage twice")
		}
		stages[string(name)] = true
		if pos, ok := st["position"]; ok && pos != nil {
			n, isInt := asInt(pos)
			if !isInt || n < MinStagePosition || n > MaxStagePosition {
				bad(at+".position", "must be "+itoa(MinStagePosition)+" to "+itoa(MaxStagePosition))
			}
		}
		if d, ok := st["description"]; ok && d != nil && utf8.RuneCountInString(asString(d)) > 500 {
			bad(at+".description", "must be at most 500 characters")
		}
	}

	pools := map[string]bool{}
	poolList := listOf(spec, "pools")
	switch {
	case len(poolList) == 0:
		bad(".pools", "needs at least one pool")
	case len(poolList) > MaxTemplatePools:
		bad(".pools", "has at most "+itoa(MaxTemplatePools)+" pools")
	}
	for i, raw := range poolList {
		at := fmt.Sprintf(".pools[%d]", i)
		pl, ok := raw.(map[string]any)
		if !ok {
			bad(at, "must be an object")
			continue
		}
		fe = append(fe, validateSpecPool(field, at, pl, stages, pools)...)
	}

	handoffs := map[[2]string]bool{}
	for i, raw := range listOf(spec, "handoffs") {
		at := fmt.Sprintf(".handoffs[%d]", i)
		h, ok := raw.(map[string]any)
		if !ok {
			bad(at, "must be an object {from, to}")
			continue
		}
		from, to := asString(h["from"]), asString(h["to"])
		switch {
		case !pools[from]:
			bad(at+".from", "must name a pool the template defines")
		case !pools[to]:
			bad(at+".to", "must name a pool the template defines")
		case from == to:
			bad(at, "a pool cannot hand off to itself")
		case handoffs[[2]string{from, to}]:
			bad(at, "repeats a handoff")
		}
		handoffs[[2]string{from, to}] = true
	}

	cfgList := listOf(spec, "configValues")
	if len(cfgList) > MaxTemplateConfigValues {
		bad(".configValues", "has at most "+itoa(MaxTemplateConfigValues)+" values")
	}
	seenCfg := map[[2]string]bool{}
	for i, raw := range cfgList {
		at := fmt.Sprintf(".configValues[%d]", i)
		c, ok := raw.(map[string]any)
		if !ok {
			bad(at, "must be an object {pool, name, value, source}")
			continue
		}
		pool := asString(c["pool"])
		if !pools[pool] {
			bad(at+".pool", "must name a pool the template defines")
		}
		name := asString(c["name"])
		if _, err := ParseConfigName(at+".name", name); err != nil {
			fe = append(fe, prefixed(field, fieldErrorsOf(err))...)
		}
		if seenCfg[[2]string{pool, name}] {
			bad(at, "repeats a value for the pool")
		}
		seenCfg[[2]string{pool, name}] = true
		src := asString(c["source"])
		if src != "" && src != "literal" && src != "namespace" {
			bad(at+".source", "must be literal or namespace")
		}
		if v, present := c["value"]; present && v != nil {
			if _, isStr := v.(string); !isStr {
				bad(at+".value", "must be a string")
			}
		}
	}

	secrets := listOf(spec, "secrets")
	if len(secrets) > MaxTemplateSecrets {
		bad(".secrets", "has at most "+itoa(MaxTemplateSecrets)+" secrets")
	}
	for i, raw := range secrets {
		at := fmt.Sprintf(".secrets[%d]", i)
		s, ok := raw.(map[string]any)
		if !ok {
			bad(at, "must be an object {name, scope?, pool?}")
			continue
		}
		if _, err := ParseName(at+".name", asString(s["name"])); err != nil {
			fe = append(fe, prefixed(field, fieldErrorsOf(err))...)
		}
		if _, hasValue := s["value"]; hasValue {
			// A template names the secrets a swarm needs; it never carries one (AGENTS.md: no secret value).
			bad(at+".value", "a template cannot hold a secret value; the secret is created empty and its value is set through PutSecretValue")
		}
		if p := asString(s["pool"]); p != "" && !pools[p] {
			bad(at+".pool", "must name a pool the template defines")
		}
	}
	return fe
}

func validateSpecPool(field, at string, pl map[string]any, stages, pools map[string]bool) []FieldError {
	var fe []FieldError
	bad := func(path, reason string) { fe = append(fe, FieldError{Field: field + at + path, Reason: reason}) }
	name, err := ParseName(at+".name", asString(pl["name"]))
	if err != nil {
		fe = append(fe, prefixed(field, fieldErrorsOf(err))...)
	} else if pools[string(name)] {
		bad(".name", "names a pool twice")
	}
	pools[string(name)] = true
	if !templateRE.MatchString(asString(pl["template"])) {
		bad(".template", "must be an agent template name (^[a-z][a-z0-9-]{1,62}$)")
	}
	if tid := asString(pl["templateId"]); tid != "" && !strings.HasPrefix(tid, string(PrefixAgentTemplate)+"_") {
		bad(".templateId", "must be a tpl_ id")
	}
	if _, err := ParseImageRef(at+".image", asString(pl["image"])); err != nil {
		fe = append(fe, prefixed(field, fieldErrorsOf(err))...)
	}
	if strings.TrimSpace(asString(pl["modelId"])) == "" {
		bad(".modelId", "is required")
	}
	if d := asString(pl["description"]); utf8.RuneCountInString(d) > 1000 {
		bad(".description", "must be at most 1000 characters")
	}
	if p := asString(pl["persona"]); utf8.RuneCountInString(p) > MaxPoolPersona {
		bad(".persona", "must be at most 10000 characters")
	}
	if st := asString(pl["stage"]); st != "" && !stages[st] {
		bad(".stage", "must name a stage the template defines")
	}
	if w, ok := pl["workers"]; ok && w != nil {
		if n, isInt := asInt(w); !isInt || n < 0 || n > MaxPoolWorkers {
			bad(".workers", "must be 0 to "+itoa(MaxPoolWorkers))
		}
	}
	if g, ok := pl["scratchSizeGiB"]; ok && g != nil {
		if n, isInt := asInt(g); !isInt || n < MinScratchGiB || n > MaxScratchGiB {
			bad(".scratchSizeGiB", "must be "+itoa(MinScratchGiB)+" to "+itoa(MaxScratchGiB))
		}
	}
	if v := asString(pl["sharedVolume"]); v != "" && !SharedVolume(v).Valid() {
		bad(".sharedVolume", "must be none or efs")
	}
	if r, ok := pl["resources"]; ok && r != nil {
		res, isMap := r.(map[string]any)
		if !isMap {
			bad(".resources", "must be an object {cpu, memory}")
		} else {
			ar := AgentResources{CPU: asString(res["cpu"]), Memory: asString(res["memory"])}
			for _, f := range ar.validate() {
				fe = append(fe, FieldError{Field: field + at + ".resources." + f.Field, Reason: f.Reason})
			}
		}
	}
	return fe
}

func prefixed(field string, fe []FieldError) []FieldError {
	out := make([]FieldError, len(fe))
	for i, f := range fe {
		out[i] = FieldError{Field: field + f.Field, Reason: f.Reason}
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asInt reads a JSON number (decoded as float64 or an integer type) that holds a whole number.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), n >= math.MinInt32 && n <= math.MaxInt32
	}
	return 0, false
}
