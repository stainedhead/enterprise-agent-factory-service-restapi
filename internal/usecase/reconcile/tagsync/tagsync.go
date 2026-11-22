// Package tagsync keeps the tags of a team's Secrets Manager secrets and SQS pool inbox queues in step
// with the effective tag set (RC-US-5 part, RC-12 part, TAG-3, B1). Both kinds of resource are tagged
// when they are created (by afs-api for secrets, by the pool controller for queues); this controller
// applies later changes: a changed team tag or service tag, a renamed agent, and the removal of a tag the
// team no longer sets (tags.Stale).
//
// The resource is the team: engine kind Kind, id = team id, one reconcile per team. The set of one
// resource is tags.Effective for its owner (team, agent or pool) plus the afs-tag-keys record, the same
// computation the team, agent and pool controllers use. A resource that does not exist in AWS (an inbox
// queue exists only for a pool that receives handoffs; a secret scheduled for deletion) is skipped. A tag
// set above the cap applies nothing to that resource and reports TagsApplied false with the limit named.
package tagsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

// Kind is the engine kind (and resource_status kind, singular) of a team's tag sync. Its id is the team id.
const Kind engine.Kind = "tag_sync"

// ErrMissing is returned by a Tagger for a resource that does not exist (or is scheduled for deletion).
var ErrMissing = errors.New("tagsync: resource does not exist")

// Secret is a secret row of the team: its ARN and its owner, when it has one.
type Secret struct {
	ARN string
	// AgentID and AgentName are set for an agent-scoped secret, PoolID and PoolName for a pool-scoped one;
	// a team-shared secret has none.
	AgentID, AgentName string
	PoolID, PoolName   string
}

// Pool is a live pool of the team; its inbox queue is tagged when it exists.
type Pool struct{ ID, Name string }

// Desired is what the reconciler reads for one team.
type Desired struct {
	TeamID, Slug string
	// State is active, suspended, deleting or deleted.
	State string
	// Tags are the team's tags beyond the system ones (service fixed and required tags and the team's own).
	Tags    map[string]string
	Secrets []Secret
	Pools   []Pool
}

// Source reads the team's secrets and pools.
type Source interface {
	// LoadTagSync returns the team, or a domain NotFound error when it does not exist.
	LoadTagSync(ctx context.Context, teamID string) (Desired, error)
}

// Tagger reads and changes the tags of one kind of AWS resource, addressed by ARN (secrets) or URL
// (queues). Implementations are idempotent and return ErrMissing for a resource that does not exist.
type Tagger interface {
	Tags(ctx context.Context, id string) (map[string]string, error)
	Tag(ctx context.Context, id string, tags map[string]string) error
	Untag(ctx context.Context, id string, keys []string) error
}

// Config configures the Controller. Every field is required.
type Config struct {
	Source  Source
	Secrets Tagger
	Queues  Tagger
	// QueueURL returns the URL of a pool's inbox queue (derived from the names; nothing is called).
	QueueURL func(teamID, poolID string) (string, error)
}

// Controller reconciles the tags of a team's secrets and queues.
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Secrets == nil || cfg.Queues == nil || cfg.QueueURL == nil {
		return nil, errors.New("tagsync: Source, Secrets, Queues and QueueURL are required")
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (c *Controller) Kind() engine.Kind { return Kind }

// Routes are the change routes of the tag sync: a team's own tags, its secrets, pools and agents (names are
// tags) reconcile that team, and a service tag change reconciles every team. mergeRoutes merges them with
// the other controllers' routes of the same tables.
func Routes() []engine.Route {
	own := []engine.Target{{Kind: Kind}}
	return []engine.Route{
		{Table: "team_tags", Targets: own},
		{Table: "secrets", Targets: own},
		{Table: "agent_pools", Targets: own},
		{Table: "agents", Targets: own},
		{Table: "service_tags", FanOut: []engine.Kind{Kind}},
	}
}

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadTagSync(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	if d.State == "deleted" || d.State == "deleting" {
		return engine.Outcome{NoStatus: true}, nil // the ordered team delete removes the resources
	}

	var changed []string
	var failed, refused []error
	apply := func(label string, tg Tagger, id string, subj tags.Subject) {
		want, err := tags.Effective(subj, d.Tags)
		if err != nil {
			refused = append(refused, fmt.Errorf("%s %s: %w", label, id, err))
			return
		}
		did, err := syncOne(ctx, tg, id, want.WithApplied())
		if err != nil {
			failed = append(failed, fmt.Errorf("%s %s: %w", label, id, err))
			return
		}
		if did {
			changed = append(changed, label+"/"+id)
		}
	}
	team := tags.Subject{TeamID: d.TeamID, TeamSlug: d.Slug}
	for _, s := range d.Secrets {
		subj := team
		subj.AgentID, subj.AgentName, subj.PoolID, subj.PoolName = s.AgentID, s.AgentName, s.PoolID, s.PoolName
		apply("Secret", c.cfg.Secrets, s.ARN, subj)
	}
	for _, p := range d.Pools {
		url, err := c.cfg.QueueURL(d.TeamID, p.ID)
		if err != nil {
			failed = append(failed, fmt.Errorf("queue of %s: %w", p.ID, err))
			continue
		}
		subj := team
		subj.PoolID, subj.PoolName = p.ID, p.Name
		apply("Queue", c.cfg.Queues, url, subj)
	}

	out := engine.Outcome{Changed: changed}
	switch {
	case len(failed) > 0:
		return out, engine.WithCondition(tags.Condition, errors.Join(append(failed, refused...)...))
	case len(refused) > 0:
		err := errors.Join(refused...)
		out.Conditions = []domain.Condition{{Type: tags.Condition, Status: domain.ConditionFalse, Reason: tags.ErrorCode, Message: err.Error()}}
		return out, engine.Terminal(engine.WithCondition(tags.Condition, err))
	}
	out.Conditions = []domain.Condition{{Type: tags.Condition, Status: domain.ConditionTrue, Reason: "Applied"}}
	return out, nil
}

// syncOne brings one resource to want (already WithApplied) and reports whether it changed anything.
func syncOne(ctx context.Context, tg Tagger, id string, want tags.Set) (bool, error) {
	have, err := tg.Tags(ctx, id)
	if errors.Is(err, ErrMissing) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read tags: %w", err)
	}
	add, stale := tags.Diff(have, want), tags.Stale(have, want)
	if len(add) > 0 {
		if err := tg.Tag(ctx, id, add); err != nil {
			return false, fmt.Errorf("tag: %w", err)
		}
	}
	if len(stale) > 0 {
		if err := tg.Untag(ctx, id, stale); err != nil {
			return false, fmt.Errorf("untag: %w", err)
		}
	}
	return len(add) > 0 || len(stale) > 0, nil
}
