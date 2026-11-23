// Package gateway is the use case of afs-gateway: identify the calling pod, check the team and the
// model, reserve the worst-case cost against the budget, invoke Bedrock through the team's inference
// profile, settle to the actual cost and record usage (gateway contract, GW-1 to GW-7).
//
// It knows neither the Bedrock wire formats nor HTTP: the adapter parses a request into a Request, and
// passes Service.Do an InvokeFunc that forwards it and reports the usage it saw.
package gateway

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ContractVersion is the gateway contract this build implements (X-Afs-Gateway-Contract-Version).
const ContractVersion = "1.0.0"

// Operation is one of the gateway paths of contract §2.
type Operation string

// Operations.
const (
	OpConverse          Operation = "converse"
	OpConverseStream    Operation = "converse-stream"
	OpInvoke            Operation = "invoke"
	OpInvokeStream      Operation = "invoke-with-response-stream"
	OpAnthropicMessages Operation = "anthropic-messages"
)

// Request is what the HTTP adapter hands to Service.Do after it parsed the wire request.
type Request struct {
	// SourceIP is the connection's source address (never a forwarded header).
	SourceIP netip.Addr
	// Token is X-Afs-Gateway-Token, empty when the runtime sent none.
	Token string
	Op    Operation
	// ModelID is the AFS model id from the path or the body.
	ModelID string
	// MaxTokens is the request's own output limit, zero when it sent none.
	MaxTokens int
	// InputBytes is the size of the request body, the basis of the input estimate.
	InputBytes int
	// RequestID identifies the call in logs, usage rows and support requests.
	RequestID string
}

// Caller is the pod behind a request, resolved from its source IP.
type Caller struct {
	TeamID    string
	Namespace string
	// AgentID or PoolID is set, never both. PodName is the pool's worker pod, or the agent's pod.
	AgentID string
	PoolID  string
	PodName string
	PodUID  string
}

// PodInfo is what the pod cache knows about an IP.
type PodInfo struct {
	Namespace      string
	Name           string
	UID            string
	ServiceAccount string
	// TeamID comes from the team namespace's label, AgentID and PoolID from the pod's labels.
	TeamID  string
	AgentID string
	PoolID  string
}

// PodDirectory maps a source IP to a pod (adapter/k8s/podcache).
type PodDirectory interface {
	// Lookup returns the current pod with the IP.
	Lookup(ip netip.Addr) (PodInfo, bool)
	// Ready reports that the initial list has been loaded; before it, an unknown IP is not an answer.
	Ready() bool
}

// TokenIdentity is the pod a reviewed token names.
type TokenIdentity struct {
	Namespace string
	PodName   string
	PodUID    string
}

// Token review errors.
var (
	// ErrTokenInvalid means the token is not valid for audience afs-gateway.
	ErrTokenInvalid = errors.New("gateway: token invalid")
	// ErrReviewUnavailable means the token could not be reviewed (the API server is unreachable).
	ErrReviewUnavailable = errors.New("gateway: token review unavailable")
)

// TokenReviewer reviews a projected ServiceAccount token (adapter/k8s/tokenreview).
type TokenReviewer interface {
	Review(ctx context.Context, token string) (TokenIdentity, error)
}

// ErrNotFound is returned by Store for a row that does not exist.
var ErrNotFound = errors.New("gateway: not found")

// TeamRecord is the part of a team the gateway needs.
type TeamRecord struct {
	ID        string
	Namespace string
	State     string
}

// ModelAccess is a team's access to a model.
type ModelAccess struct {
	Enabled         bool
	ProfileARN      string
	MaxOutputTokens int
}

// Price is a model price row; amounts are USD per million tokens.
type Price struct {
	EffectiveFrom                        time.Time
	Input, Output, CacheRead, CacheWrite domain.Money
}

// BudgetRecord is a team's budget.
type BudgetRecord struct {
	ID         string
	Period     string // monthly or lifetime
	StartsAt   time.Time
	Thresholds []int
	OnExhaust  string // block or notify
	Amount     domain.Money
}

// Tag is one effective tag of a team.
type Tag struct {
	Key, Value, Source string
	// Service is true when the key is one of the service tags (fixed or required), whoever set the value:
	// a required key the team filled in is still ordered with the service tags (D27).
	Service bool
}

// Store reads the desired state the gateway needs (afs_gateway role; database contract §2).
type Store interface {
	Team(ctx context.Context, teamID string) (TeamRecord, error)
	// TeamModel returns the team's inference profile for the model (team_inference_profiles); ErrNotFound when
	// none exists yet.
	TeamModel(ctx context.Context, teamID, modelID string) (ModelAccess, error)
	// ModelAdmission answers, for one agent or pool (kind "agent" or "pool"), why each catalog model is refused:
	// the map holds every model, with an empty reason for an admitted one (afs.model_admission). ErrNotFound
	// when the subject does not exist.
	ModelAdmission(ctx context.Context, kind, subjectID string) (map[string]string, error)
	// Prices returns the model's prices, newest effective_from first.
	Prices(ctx context.Context, modelID string) ([]Price, error)
	// Budget returns the team's budget, or ErrNotFound.
	Budget(ctx context.Context, teamID string) (BudgetRecord, error)
	Tags(ctx context.Context, teamID string) ([]Tag, error)
	// TagSetID returns the id of the team's current tag set (afs.current_team_tag_set).
	TagSetID(ctx context.Context, teamID string) (string, error)
}

// ModelDenialRecorder is told of every call refused by model admission; the composition root binds it to
// afs_gateway_model_denials_total{reason} (gateway contract §7).
type ModelDenialRecorder interface {
	ModelDenied(teamID, modelID, reason string)
}

// Hold is the result of a reservation attempt.
type Hold struct {
	// ID is the reservation id; empty when the reservation did not fit.
	ID string
	// Limit, Spent and Reserved are the period's counter, after a refusal (for remainingUsd).
	Limit, Spent, Reserved domain.Money
}

// Counter is a period's counter after a settlement.
type Counter struct {
	Limit, Spent, Reserved domain.Money
}

// Spend is the budget counter port (afs.ensure_spend_counter, reserve_spend_held, settle_reservation).
type Spend interface {
	EnsureCounter(ctx context.Context, budgetID string, period time.Time) error
	Reserve(ctx context.Context, budgetID string, period time.Time, amount domain.Money, holder string) (Hold, error)
	// Settle settles the hold with the budget and period it reserved against. An empty holdID settles
	// no hold and adds the actual cost (a notify-only budget that refused nothing).
	Settle(ctx context.Context, holdID, budgetID string, period time.Time, actual domain.Money) (Counter, error)
}

// Crossing is a threshold event to record once per budget period.
type Crossing struct {
	TeamID, BudgetID string
	Period           time.Time
	// Threshold is the percentage; ExhaustedMarker marks the budget.exhausted event.
	Threshold int
	Event     domain.EventType
	Data      map[string]any
}

// ExhaustedMarker is the threshold value that dedupes budget.exhausted in budget_threshold_events.
const ExhaustedMarker = 101

// Thresholds records crossings insert-once (budget_threshold_events) together with the outbox event.
type Thresholds interface {
	// Record inserts the crossing and its outbox event in one transaction when the row is new. It
	// reports whether this caller inserted it.
	Record(ctx context.Context, c Crossing) (first bool, err error)
}

// Outcome of a call as usage_events records it.
type Outcome string

// Outcomes.
const (
	OutcomeOK             Outcome = "ok"
	OutcomeError          Outcome = "error"
	OutcomeRejectedBudget Outcome = "rejected_budget"
	OutcomeRejectedModel  Outcome = "rejected_model"
)

// Usage is the token usage of a call.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int
}

// Record is one usage_events row.
type Record struct {
	OccurredAt time.Time
	TeamID     string
	AgentID    string
	PoolID     string
	WorkerPod  string
	ModelID    string
	RequestID  string
	Outcome    Outcome
	Usage      Usage
	Cost       domain.Money
	LatencyMS  int
	TagSetID   string
}

// UsageSink accepts usage rows. Record never blocks the call: the writer batches (gateway contract §8).
type UsageSink interface {
	Record(r Record)
}

// Authorized is what an InvokeFunc needs to forward the call.
type Authorized struct {
	Caller     Caller
	ModelID    string
	ProfileARN string
	// RoleARN is the team's Bedrock role the gateway assumes.
	RoleARN string
	// MaxOutputTokens is the cap the request's own limit must not exceed (gateway contract §4).
	MaxOutputTokens int
	// Metadata is the requestMetadata the call carries; the agent's own is replaced (contract §6).
	Metadata map[string]string
	// DroppedTags are the tag keys that did not fit the 16 pairs.
	DroppedTags []string
	RequestID   string
}

// Result is what an InvokeFunc reports.
type Result struct {
	Usage Usage
	// UsageReported is true when the upstream reported usage (the final metadata event, the response body
	// or its headers).
	UsageReported bool
	// Started is true when the upstream began to answer: for a stream, once its first event arrived.
	Started bool
}

// InvokeFunc forwards the call and reports what it used. It returns a domain error (Throttled,
// Upstream, Unavailable) for an upstream failure; a Result alongside the error says how far it got.
type InvokeFunc func(ctx context.Context, a Authorized) (Result, error)
