package domain

import (
	"regexp"
	"strings"
	"time"
)

// Limits of the model catalog (database models and model_prices).
const (
	maxContextWindow = 10_000_000
	// maxPriceMicros is the largest value of numeric(12,6): 999999.999999 USD per million tokens.
	maxPriceMicros = 999_999_999_999
)

// modelIDRE accepts Bedrock model ids and inference profile ids, including the ":0" version suffix
// (the router splits a trailing ":word" off a path segment only when it is a registered action verb).
var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// Model is a Bedrock model in the catalog. MaxOutputTokens is the gateway's worst case when a
// request sets no max tokens (D28), so it is required on create.
type Model struct {
	ID              string
	Provider        string
	DisplayName     string
	Enabled         bool
	ContextWindow   int
	MaxOutputTokens int
	Version         Version
	UpdatedAt       time.Time
	// ServiceEnabled is true while the service may use the model: enabled, or the service's model access
	// is all (MOD-1).
	ServiceEnabled bool
	// Callable is true while the model is enabled for the service and has a price in force.
	Callable bool
	// PriceState is approved (a price is in force), pending (a proposal or a future price) or none.
	PriceState PriceState
	// Origin is manual, or bedrock_sync for a model the daily sync found.
	Origin ModelOrigin
	// TeamsWithAccess counts the teams that may use the model (the admin list only; -1 when not loaded).
	TeamsWithAccess int
	// ProviderOff is true while the Bedrock provider is switched off (v1.5.0 providerState off); the
	// model is then not callable.
	ProviderOff bool
}

// ProviderState is the v1.5.0 providerState of a model: on or off.
func ProviderState(off bool) string {
	if off {
		return "off"
	}
	return "on"
}

// PriceState says whether a model has a price the gateway can charge.
type PriceState string

// Price states of a model.
const (
	PriceApproved PriceState = "approved"
	PricePending  PriceState = "pending"
	PriceNone     PriceState = "none"
)

// ModelOrigin says how a model entered the catalog.
type ModelOrigin string

// Model origins (database models.origin).
const (
	OriginManual      ModelOrigin = "manual"
	OriginBedrockSync ModelOrigin = "bedrock_sync"
)

// ServiceModelAccess is the service-wide model access: every catalog model, or the enabled ones
// (setting models.service_access, MOD-1).
type ServiceModelAccess struct {
	Access        ModelAccess
	EnabledModels int
	// CallableModels counts the models that are enabled for the service and have a price in force.
	CallableModels int
	Version        Version
}

// NewModelParams are the fields of CreateModel. Enabled defaults to true.
type NewModelParams struct {
	ID              string
	Provider        string
	DisplayName     string
	Enabled         *bool
	ContextWindow   int
	MaxOutputTokens int
}

func validateModelFields(provider, displayName *string, ctxWindow, maxOut *int) []FieldError {
	var fe []FieldError
	if provider != nil && (strings.TrimSpace(*provider) == "" || len(*provider) > 100) {
		fe = append(fe, FieldError{Field: "provider", Reason: "is required and at most 100 characters"})
	}
	if displayName != nil && (strings.TrimSpace(*displayName) == "" || len(*displayName) > 200) {
		fe = append(fe, FieldError{Field: "displayName", Reason: "is required and at most 200 characters"})
	}
	if ctxWindow != nil && (*ctxWindow < 1 || *ctxWindow > maxContextWindow) {
		fe = append(fe, FieldError{Field: "contextWindow", Reason: "must be from 1 to 10000000"})
	}
	if maxOut != nil && (*maxOut < 1 || *maxOut > maxContextWindow) {
		fe = append(fe, FieldError{Field: "maxOutputTokens", Reason: "is required: a positive number of tokens (D28)"})
	}
	return fe
}

// NewModel validates p and returns the model.
func NewModel(p NewModelParams) (Model, error) {
	fe := validateModelFields(&p.Provider, &p.DisplayName, &p.ContextWindow, &p.MaxOutputTokens)
	if !modelIDRE.MatchString(p.ID) {
		fe = append([]FieldError{{Field: "id", Reason: "must be a Bedrock model id of letters, digits, '.', '-', '_', ':' or '/'"}}, fe...)
	}
	if len(fe) == 0 && p.MaxOutputTokens > p.ContextWindow {
		fe = append(fe, FieldError{Field: "maxOutputTokens", Reason: "must not exceed contextWindow"})
	}
	if len(fe) > 0 {
		return Model{}, Validation(fe...)
	}
	m := Model{ID: p.ID, Provider: p.Provider, DisplayName: p.DisplayName, Enabled: true, ContextWindow: p.ContextWindow,
		MaxOutputTokens: p.MaxOutputTokens}
	if p.Enabled != nil {
		m.Enabled = *p.Enabled
	}
	return m, nil
}

// ModelUpdate are the fields of UpdateModel; nil leaves a field as it is. The id and provider are
// fixed once the model exists.
type ModelUpdate struct {
	DisplayName     *string
	Enabled         *bool
	ContextWindow   *int
	MaxOutputTokens *int
}

// Apply returns m with u applied, or a ValidationError.
func (m Model) Apply(u ModelUpdate) (Model, error) {
	if u.DisplayName == nil && u.Enabled == nil && u.ContextWindow == nil && u.MaxOutputTokens == nil {
		return Model{}, Validation(FieldError{Field: "body", Reason: "at least one field is required"})
	}
	fe := validateModelFields(nil, u.DisplayName, u.ContextWindow, u.MaxOutputTokens)
	if u.DisplayName != nil {
		m.DisplayName = *u.DisplayName
	}
	if u.Enabled != nil {
		m.Enabled = *u.Enabled
	}
	if u.ContextWindow != nil {
		m.ContextWindow = *u.ContextWindow
	}
	if u.MaxOutputTokens != nil {
		m.MaxOutputTokens = *u.MaxOutputTokens
	}
	if len(fe) == 0 && m.MaxOutputTokens > m.ContextWindow && m.ContextWindow > 0 {
		fe = append(fe, FieldError{Field: "maxOutputTokens", Reason: "must not exceed contextWindow"})
	}
	if len(fe) > 0 {
		return Model{}, Validation(fe...)
	}
	return m, nil
}

// PriceSource says where a price came from.
type PriceSource string

// Price sources (database model_prices.source).
const (
	PriceSourceManual       PriceSource = "manual"
	PriceSourceAWSPriceList PriceSource = "aws_price_list"
	PriceSourceCURAdjusted  PriceSource = "cur_adjusted"
)

// ModelPrice is the price per million tokens of one model from EffectiveFrom on. Prices are
// effective-dated and never edited: a new price is a new row.
type ModelPrice struct {
	ModelID       string
	EffectiveFrom time.Time
	Input         Money
	Output        Money
	CacheRead     Money
	CacheWrite    Money
	Source        PriceSource
	CreatedAt     time.Time
	CreatedBy     string
}

// NewModelPriceParams are the fields of CreateModelPrice; nil cache prices default to zero.
type NewModelPriceParams struct {
	EffectiveFrom time.Time
	Input         Money
	Output        Money
	CacheRead     *Money
	CacheWrite    *Money
}

// NewModelPrice validates p for modelID. A manual price is always source manual.
func NewModelPrice(modelID string, p NewModelPriceParams) (ModelPrice, error) {
	var fe []FieldError
	if p.EffectiveFrom.IsZero() {
		fe = append(fe, FieldError{Field: "effectiveFrom", Reason: "is required"})
	}
	check := func(field string, m Money) {
		if m.IsNegative() || m.Micros() > maxPriceMicros {
			fe = append(fe, FieldError{Field: field, Reason: "must be from 0 to 999999.999999"})
		}
	}
	check("inputUsdPerMtok", p.Input)
	check("outputUsdPerMtok", p.Output)
	out := ModelPrice{ModelID: modelID, EffectiveFrom: p.EffectiveFrom.UTC(), Input: p.Input, Output: p.Output, Source: PriceSourceManual}
	if p.CacheRead != nil {
		check("cacheReadUsdPerMtok", *p.CacheRead)
		out.CacheRead = *p.CacheRead
	}
	if p.CacheWrite != nil {
		check("cacheWriteUsdPerMtok", *p.CacheWrite)
		out.CacheWrite = *p.CacheWrite
	}
	if len(fe) > 0 {
		return ModelPrice{}, Validation(fe...)
	}
	return out, nil
}

// ParseServiceModelAccess validates the body of PutServiceModelAccess.
func ParseServiceModelAccess(access string) (ModelAccess, error) {
	a := ModelAccess(access)
	if a != ModelAccessAll && a != ModelAccessSelected {
		return "", Validation(FieldError{Field: "access", Reason: "must be all or selected"})
	}
	return a, nil
}
