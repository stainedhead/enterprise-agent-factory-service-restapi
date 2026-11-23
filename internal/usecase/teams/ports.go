// Package teams implements the team operations of API contract §4.2: create, read, update,
// suspend, resume, delete, models and tags. Every mutation writes the row, an audit record and (for
// the event types of contract §7) an outbox event in one transaction.
package teams

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ListFilter filters ListTeams.
type ListFilter struct {
	State domain.TeamState
	Kind  domain.TeamKind
	// HideDeleted drops teams in state deleted (everyone but service admins asking for them).
	HideDeleted bool
}

// BudgetInput is the lifetime budget an ephemeral team is created with.
type BudgetInput struct {
	ID         string
	Amount     domain.Money
	Thresholds []int
}

// ModelInfo is a model a team may use (API contract §5 Model).
type ModelInfo struct {
	ID              string
	Provider        string
	DisplayName     string
	Enabled         bool
	ContextWindow   int
	MaxOutputTokens int
	Version         int64
	UpdatedAt       time.Time
	// Callable is true while the model has a price in force (the gateway needs a price, MOD-1).
	Callable bool
	// PriceState is none, pending or approved.
	PriceState string
	// InferenceProfileState is the team's inference profile state for the model (creating, active or
	// retiring); empty when there is none.
	InferenceProfileState string
}

// TeamModels is a team's model access: all or the stored selection, and the effective list.
type TeamModels struct {
	Access   domain.ModelAccess
	ModelIDs []string
	// Items are the models the team may use now: the service's models, narrowed by the selection.
	Items   []ModelInfo
	Version domain.Version
}

// OrchestrationInput is the orchestration object of CreateTeam (ORC-26); every member is optional.
type OrchestrationInput struct {
	Tool             domain.CollabTool
	WebUIExposure    domain.Exposure
	WebAPIExposure   domain.Exposure
	RegistrationMode domain.RegistrationMode
	Models           *domain.ModelsInput
	ModelID          string
	Resources        *domain.AgentResources
	VolumeSizeGiB    int
	VolumeOnDelete   string
}

// OrchestrationAgent is what create_orchestration_agent is called with; empty members take the
// service defaults inside the database function (afs.create_orchestration_agent).
type OrchestrationAgent struct {
	TeamID, AgentID, Name         string
	Tool                          domain.CollabTool
	WebUIExposure, WebAPIExposure domain.Exposure
	RegistrationMode              domain.RegistrationMode
	Models                        domain.AgentModels
	Resources                     *domain.AgentResources
	VolumeSizeGiB                 int
	VolumeOnDelete                string
}

// Repository is the team store. Implementations run inside the transaction they are given, so
// row-level security applies with the caller's session.
type Repository interface {
	// Insert stores a new team; a duplicate slug is a Conflict.
	Insert(ctx context.Context, tx port.Tx, t domain.Team) error
	// Get returns the team with its reconcile status; NotFound when absent or invisible. forUpdate
	// locks the row.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.Team, error)
	// List returns up to limit teams with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, f ListFilter, after string, limit int) ([]domain.Team, error)
	// Update writes every mutable column; zero rows is PreconditionFailed (the row exists, the
	// version is not expect).
	Update(ctx context.Context, tx port.Tx, t domain.Team, expect domain.Version) (domain.Team, error)
	// BudgetAmount is the team's budget amount, nil when it has none.
	BudgetAmount(ctx context.Context, tx port.Tx, teamID string) (*domain.Money, error)
	// InsertBudget stores the lifetime budget of an ephemeral team.
	InsertBudget(ctx context.Context, tx port.Tx, teamID string, b BudgetInput) error
	// ResolveUser returns the id of the user with this id or email; ok false when there is none.
	ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (id string, ok bool, err error)
	// InsertAdminBinding stores the first team-admin binding.
	InsertAdminBinding(ctx context.Context, tx port.Tx, bindingID, teamID string, m domain.NewMemberParams) error
	// Models returns a team's model access, stored selection and the models it may use now. The version
	// is the team's.
	Models(ctx context.Context, tx port.Tx, teamID string) (TeamModels, error)
	// ModelsByID returns the catalog models with these ids (missing ids are absent).
	ModelsByID(ctx context.Context, tx port.Tx, ids []string) (map[string]ModelInfo, error)
	// ServiceModelIDs returns the ids enabled for the service (every catalog model while the service's
	// access is all).
	ServiceModelIDs(ctx context.Context, tx port.Tx) ([]string, error)
	// ReplaceModels makes the team's stored model selection exactly ids.
	ReplaceModels(ctx context.Context, tx port.Tx, teamID string, ids []string) error
	// SetModelAccess sets the team's model access (service admins only).
	SetModelAccess(ctx context.Context, tx port.Tx, teamID string, access domain.ModelAccess) error
	// ModelSets returns the sets an agent's models are checked against, for the team.
	ModelSets(ctx context.Context, tx port.Tx, teamID string) (domain.ModelSets, error)
	// InsertOrchestration calls afs.create_orchestration_agent for the team just inserted.
	InsertOrchestration(ctx context.Context, tx port.Tx, in OrchestrationAgent) error
	// WithoutOrchestration returns up to limit ids of live teams (not deleting or deleted) that have no live
	// orchestration agent, in id order.
	WithoutOrchestration(ctx context.Context, tx port.Tx, limit int) ([]string, error)
	// HasOrchestration reports whether the team has a live orchestration agent.
	HasOrchestration(ctx context.Context, tx port.Tx, teamID string) (bool, error)
	// ServiceTags lists the service tags.
	ServiceTags(ctx context.Context, tx port.Tx) ([]domain.ServiceTag, error)
	// TeamTags lists the team's own tags and the version of the set.
	TeamTags(ctx context.Context, tx port.Tx, teamID string) ([]domain.TagValue, domain.Version, error)
	// ReplaceTeamTags makes the team's own tags exactly values.
	ReplaceTeamTags(ctx context.Context, tx port.Tx, teamID string, values []domain.TagValue) error
}
