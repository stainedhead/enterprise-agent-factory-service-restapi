// Package links reconciles the cross-account role links of a team's dedicated agents (RC-3, AGT-10,
// M4-A4, M4-A5): it verifies each link by assuming it with the external id and session tags, writes
// pending_trust, verified or failed (with the AWS error code) back to the link, publishes
// role_link.verified and role_link.failed on a change of status, and disables links whose account the
// team's allowed-accounts filter no longer allows (and enables them again when it allows it).
//
// The resource is the team (engine kind Kind, id = team id): every link of the team is looked at in one
// pass. A link is verified when it is pending_trust (created, or verified on demand), when a failed one
// is FailedRetry old and when a verified one is VerifyInterval old (the daily check).
//
// How a link is verified: the reconciler assumes the agent's own dedicated role (whose trust policy
// names the reconciler's role, see TrustWithVerifier) and, with that session, assumes the link's role
// with the external id, the session name set to the agent id (D51) and the session tags afs-team-id and
// afs-agent-id. The target account's trust policy names the agent's role, so a success proves the trust
// the agent itself will rely on. The agent role's sts:AssumeRole statements are built by
// AssumeStatement and written by the agent controller.
package links

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Kind is the engine kind of a team's role links. Its id is the team id.
const Kind engine.Kind = "links"

// Timing of verification.
const (
	// VerifyInterval is how often a verified link is checked again (daily).
	VerifyInterval = 24 * time.Hour
	// FailedRetry is how long a failed link waits before the reconciler tries it again; verifying on
	// demand tries at once.
	FailedRetry = time.Hour
)

// Error codes the reconciler records itself.
const (
	CodeVerifierNotConfigured = "VerifierNotConfigured"
	CodeNotAllowed            = "AccountNotAllowed"
	maxErrorLen               = 300
)

// Link is one role link as the reconciler sees it.
type Link struct {
	ID           string
	AgentID      string
	AccountID    string
	RoleARN      string
	ProfileName  string
	ExternalID   string
	MaxSession   int
	Status       domain.RoleLinkStatus
	LastVerified *time.Time
	// UpdatedAt is when the row last changed; for a failed link it is when it failed.
	UpdatedAt time.Time
	// Allowed is afs.account_allowed(team, account): the account is active and inside the team's filter.
	Allowed bool
	// AgentDedicated is true when the agent has its own IAM role; AgentLive is false for an agent that is
	// being deleted.
	AgentDedicated bool
	AgentLive      bool
	// Namespace is the team's namespace (the agent role's path).
	Namespace string
}

// Desired is a team's links.
type Desired struct {
	TeamID string
	// State is active, suspended, deleting or deleted.
	State string
	Links []Link
}

// Source loads a team's links.
type Source interface {
	LoadLinks(ctx context.Context, teamID string) (Desired, error)
}

// Change is a status written back to a link.
type Change struct {
	TeamID  string
	AgentID string
	LinkID  string
	// From and To are the statuses before and after.
	From, To   domain.RoleLinkStatus
	VerifiedAt *time.Time
	Error      string
	AccountID  string
	RoleARN    string
	// Event is the contract event to publish with the change, or "".
	Event domain.EventType
}

// Recorder writes a change: the status columns, an audit record and the event, in one transaction.
type Recorder interface {
	Record(ctx context.Context, c Change) error
}

// VerifyRequest is what a probe needs to assume one link.
type VerifyRequest struct {
	TeamID, AgentID string
	AgentRoleARN    string
	TargetRoleARN   string
	ExternalID      string
	// SessionName is the role session name of the link's assumption: the agent id (D51).
	SessionName string
	// DurationSeconds is the link's maximum session length.
	DurationSeconds int
}

// Probe assumes a link's role. A nil error means the link works. An error that carries an AWS error code
// (ErrorCode() string, as the SDK's API errors do) is recorded with that code.
type Probe interface {
	Verify(ctx context.Context, r VerifyRequest) error
}

// Config configures the Controller.
type Config struct {
	Source   Source
	Recorder Recorder
	// Probe verifies a link; nil records CodeVerifierNotConfigured.
	Probe Probe
	// AgentRoleARN names an agent's dedicated role (the same formula as the agent controller).
	AgentRoleARN func(namespace, agentID string) string
	Now          func() time.Time
}

// Controller implements engine.Controller for Kind.
type Controller struct{ cfg Config }

var _ engine.Controller = (*Controller)(nil)

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Recorder == nil || cfg.AgentRoleARN == nil {
		return nil, errors.New("links: Source, Recorder and AgentRoleARN are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (*Controller) Kind() engine.Kind { return Kind }

// Routes returns the table routes of the links controller. The agents table is routed too: an agent
// that becomes dedicated, or is deleted, changes what its links can do.
func Routes() []engine.Route {
	own := []engine.Target{{Kind: Kind}}
	return []engine.Route{
		{Table: "agent_role_links", Targets: own},
		{Table: "teams", Targets: own},
		{Table: "agents", Targets: own},
		{Table: "org_aws_accounts", FanOut: []engine.Kind{Kind}},
	}
}

// due reports whether the link is to be verified now, and when it next will be otherwise.
func due(l Link, now time.Time) (bool, time.Duration) {
	switch l.Status {
	case domain.RoleLinkStatusPendingTrust:
		return true, 0
	case domain.RoleLinkStatusFailed:
		if wait := l.UpdatedAt.Add(FailedRetry).Sub(now); wait > 0 {
			return false, wait
		}
		return true, 0
	case domain.RoleLinkStatusVerified:
		at := l.UpdatedAt
		if l.LastVerified != nil {
			at = *l.LastVerified
		}
		if wait := at.Add(VerifyInterval).Sub(now); wait > 0 {
			return false, wait
		}
		return true, 0
	}
	return false, 0
}

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadLinks(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	out := engine.Outcome{NoStatus: true}
	if d.State == "deleting" || d.State == "deleted" || len(d.Links) == 0 {
		return out, nil
	}
	now := c.cfg.Now().UTC()
	var errs []error
	var next time.Duration
	soonest := func(w time.Duration) {
		if w > 0 && (next == 0 || w < next) {
			next = w
		}
	}
	for _, l := range d.Links {
		wait, err := c.reconcileLink(ctx, d, l, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("link %s: %w", l.ID, err))
		}
		soonest(wait)
	}
	out.Requeue = next
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// reconcileLink handles one link and returns how long until it needs looking at again (0: only on change).
func (c *Controller) reconcileLink(ctx context.Context, d Desired, l Link, now time.Time) (time.Duration, error) {
	base := Change{TeamID: d.TeamID, AgentID: l.AgentID, LinkID: l.ID, From: l.Status, AccountID: l.AccountID, RoleARN: l.RoleARN}
	if !l.Allowed {
		// RC-US-4.2: narrowing the allowed-accounts filter disables the links outside it on this pass.
		if l.Status == domain.RoleLinkStatusDisabled {
			return 0, nil
		}
		base.To, base.Error = domain.RoleLinkStatusDisabled, CodeNotAllowed+": the account is not an active account in the team's allowed accounts"
		return 0, c.cfg.Recorder.Record(ctx, base)
	}
	if l.Status == domain.RoleLinkStatusDisabled {
		// The filter allows the account again: verify it afresh.
		l.Status = domain.RoleLinkStatusPendingTrust
		base.To = domain.RoleLinkStatusPendingTrust
		if err := c.cfg.Recorder.Record(ctx, base); err != nil {
			return 0, err
		}
		base.From = domain.RoleLinkStatusPendingTrust
	}
	if d.State == "suspended" || !l.AgentLive || !l.AgentDedicated {
		return 0, nil
	}
	ok, wait := due(l, now)
	if !ok {
		return wait, nil
	}
	ch := base
	ch.VerifiedAt = &now
	err := c.verify(ctx, d, l)
	switch {
	case err == nil:
		ch.To = domain.RoleLinkStatusVerified
		if l.Status != domain.RoleLinkStatusVerified {
			ch.Event = domain.EventRoleLinkVerified
		}
	default:
		ch.To, ch.VerifiedAt, ch.Error = domain.RoleLinkStatusFailed, nil, describe(err)
		if l.Status != domain.RoleLinkStatusFailed {
			ch.Event = domain.EventRoleLinkFailed
		}
	}
	if err := c.cfg.Recorder.Record(ctx, ch); err != nil {
		return 0, err
	}
	if ch.To == domain.RoleLinkStatusVerified {
		return VerifyInterval, nil
	}
	return FailedRetry, nil
}

func (c *Controller) verify(ctx context.Context, d Desired, l Link) error {
	if c.cfg.Probe == nil {
		return codeError(CodeVerifierNotConfigured)
	}
	max := l.MaxSession
	if max == 0 {
		max = domain.DefaultRoleLinkSessionSeconds
	}
	return c.cfg.Probe.Verify(ctx, VerifyRequest{
		TeamID: d.TeamID, AgentID: l.AgentID, AgentRoleARN: c.cfg.AgentRoleARN(l.Namespace, l.AgentID),
		TargetRoleARN: l.RoleARN, ExternalID: l.ExternalID, SessionName: l.AgentID, DurationSeconds: max,
	})
}

type codeError string

func (e codeError) Error() string     { return string(e) }
func (e codeError) ErrorCode() string { return string(e) }

// describe is the text recorded as a link's last error: the AWS error code, then the message, never
// longer than maxErrorLen. Credentials are not in AWS error messages, and the external id is a guard.
func describe(err error) string {
	var coded interface{ ErrorCode() string }
	msg := err.Error()
	if errors.As(err, &coded) {
		if code := coded.ErrorCode(); code != "" {
			if msg == code {
				return code
			}
			msg = code + ": " + msg
		}
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	return msg
}
