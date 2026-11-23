// Package pools implements the swarm operations of API contract §4.4a (RA-8, M5-B): stages, pools,
// scale, pause and resume, handoffs, pool secret bindings, configuration values, external systems and
// shared role links. Pools exist only in ephemeral teams: every write checks the team's kind first and
// answers 409 on a persistent team, so the database's 23514 is only a backstop. The API writes desired
// state; the reconciler (M5-C) renders the Deployment, queues and identity from it.
package pools

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	// StatusKind is the resource_kind the reconciler writes pool status under (singular, like team and agent).
	StatusKind = "pool"

	kindPool    = "agent_pools"
	kindStage   = "swarm_stages"
	kindConfig  = "pool_config_values"
	kindLink    = "pool_role_links"
	kindHandoff = "pool_handoffs"
	kindBinding = "pool_secret_bindings"
	kindOptOut  = "external_system_optouts"
)

// TemplateInfo is a catalog agent template as the checks need it.
type TemplateInfo struct {
	ID             string
	Name           string
	Enabled        bool
	AllowedDigests []string
}

// SecretInfo says who owns a secret: an agent, a pool, or neither (team shared).
type SecretInfo struct {
	AgentID string
	PoolID  string
}

// PoolRepository is the pool store with the catalog reads CreatePool checks against.
type PoolRepository interface {
	// TeamKind returns the team's kind; NotFound when the caller cannot see the team.
	TeamKind(ctx context.Context, tx port.Tx, teamID string) (domain.TeamKind, error)
	// List returns the team's pools that are not deleted, with id greater than after, in id order,
	// handoffs included.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Pool, error)
	// Get returns one pool that is not deleted; NotFound otherwise. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Pool, error)
	// Insert stores a new pool and the catalog template reference.
	Insert(ctx context.Context, tx port.Tx, p domain.Pool, templateID string) error
	// Update stores the pool's spec, state and generation. Template and image are written only when
	// templateChanged, so the database's template guard does not run for any other change.
	// PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, p domain.Pool, templateID string, templateChanged bool, expect domain.Version) error
	// Touch raises the generation of the pools (a satellite changed that the reconciler must render
	// again) and so their version.
	Touch(ctx context.Context, tx port.Tx, teamID string, poolIDs []string) error
	Template(ctx context.Context, tx port.Tx, name string) (TemplateInfo, bool, error)
	// ModelSets returns the sets the models of a pool are checked against (MOD-3).
	ModelSets(ctx context.Context, tx port.Tx, teamID string) (domain.ModelSets, error)
	// EffectiveModels fills Models.Effective of each pool (service ∩ team ∩ pool ∩ callable).
	EffectiveModels(ctx context.Context, tx port.Tx, teamID string, list []domain.Pool) error
	// AgentNameTaken reports whether a live agent of the team has this name.
	AgentNameTaken(ctx context.Context, tx port.Tx, teamID, name string) (bool, error)
	// Workers returns the worker counts of the pool's latest activity window.
	Workers(ctx context.Context, tx port.Tx, teamID, poolID string) (domain.PoolWorkers, error)
}

// StageRepository is the swarm_stages store.
type StageRepository interface {
	ListStages(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Stage, error)
	GetStage(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Stage, error)
	// InsertStage stores a stage; a duplicate name or position is a Conflict.
	InsertStage(ctx context.Context, tx port.Tx, s domain.Stage) error
	// UpdateStage stores position and description; PreconditionFailed when the version is not expect.
	UpdateStage(ctx context.Context, tx port.Tx, s domain.Stage, expect domain.Version) error
	// DeleteStage removes a stage; a stage with pools is a Conflict.
	DeleteStage(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error
}

// SatelliteRepository holds what hangs off a pool: handoffs, secret bindings, configuration, external
// system switches and role links.
type SatelliteRepository interface {
	// Handoffs lists the pool ids the pool hands work to, sorted.
	Handoffs(ctx context.Context, tx port.Tx, teamID, poolID string) ([]string, error)
	// PoolsExist returns how many of the ids are live pools of the team.
	PoolsExist(ctx context.Context, tx port.Tx, teamID string, ids []string) (int, error)
	// ReplaceHandoffs makes the pool's handoffs exactly to.
	ReplaceHandoffs(ctx context.Context, tx port.Tx, teamID, poolID string, to []string) error

	Bindings(ctx context.Context, tx port.Tx, teamID, poolID string) ([]domain.SecretBinding, error)
	// Secrets returns the owners of the team's secrets with these ids; unknown ids are absent.
	Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]SecretInfo, error)
	// ReplaceBindings makes the pool's bindings exactly bs.
	ReplaceBindings(ctx context.Context, tx port.Tx, teamID, poolID string, bs []domain.SecretBinding) error

	ListConfig(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.PoolConfigValue, error)
	GetConfig(ctx context.Context, tx port.Tx, teamID, poolID string, name domain.ConfigName, forUpdate bool) (domain.PoolConfigValue, error)
	// NamespaceValueExists reports whether the team has a namespace value of that name.
	NamespaceValueExists(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName) (bool, error)
	InsertConfig(ctx context.Context, tx port.Tx, v domain.PoolConfigValue) error
	UpdateConfig(ctx context.Context, tx port.Tx, v domain.PoolConfigValue, expect domain.Version) error
	DeleteConfig(ctx context.Context, tx port.Tx, teamID, poolID string, name domain.ConfigName, expect domain.Version) error

	ListSystems(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.ExternalSystemAccess, error)
	GetSystem(ctx context.Context, tx port.Tx, teamID, poolID, systemID string) (domain.ExternalSystemAccess, error)
	// SetSystemOff switches the system off (true) or on (false) for the pool; idempotent both ways.
	SetSystemOff(ctx context.Context, tx port.Tx, teamID, poolID, systemID, optOutID string, off bool) error

	// AccountAllowed reports afs.account_allowed(team, account).
	AccountAllowed(ctx context.Context, tx port.Tx, teamID, accountID string) (bool, error)
	// LinkRoleName returns the one role name the pool's existing links share; "" when it has none.
	LinkRoleName(ctx context.Context, tx port.Tx, teamID, poolID string) (string, error)
	ListLinks(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.PoolRoleLink, error)
	GetLink(ctx context.Context, tx port.Tx, teamID, poolID, linkID string, forUpdate bool) (domain.PoolRoleLink, error)
	// InsertLink stores a link; a second link in an account or under a profile name is a Conflict.
	InsertLink(ctx context.Context, tx port.Tx, l domain.PoolRoleLink) error
	// DeleteLink removes a link; PreconditionFailed when the version is not expect.
	DeleteLink(ctx context.Context, tx port.Tx, teamID, poolID, linkID string, expect domain.Version) error
	// RequestVerify sets the link back to pending_trust (clearing the last error) and returns it.
	RequestVerify(ctx context.Context, tx port.Tx, teamID, poolID, linkID string) (domain.PoolRoleLink, error)
}

// Repository is everything the pool use cases need.
type Repository interface {
	PoolRepository
	StageRepository
	SatelliteRepository
}
