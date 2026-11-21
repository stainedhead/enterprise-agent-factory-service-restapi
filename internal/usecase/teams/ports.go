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
	// Models lists a team's models and the version of the set.
	Models(ctx context.Context, tx port.Tx, teamID string) ([]ModelInfo, domain.Version, error)
	// ModelsByID returns the catalog models with these ids (missing ids are absent).
	ModelsByID(ctx context.Context, tx port.Tx, ids []string) (map[string]ModelInfo, error)
	// ReplaceModels makes the team's model set exactly ids.
	ReplaceModels(ctx context.Context, tx port.Tx, teamID string, ids []string) error
	// ServiceTags lists the service tags.
	ServiceTags(ctx context.Context, tx port.Tx) ([]domain.ServiceTag, error)
	// TeamTags lists the team's own tags and the version of the set.
	TeamTags(ctx context.Context, tx port.Tx, teamID string) ([]domain.TagValue, domain.Version, error)
	// ReplaceTeamTags makes the team's own tags exactly values.
	ReplaceTeamTags(ctx context.Context, tx port.Tx, teamID string, values []domain.TagValue) error
}
