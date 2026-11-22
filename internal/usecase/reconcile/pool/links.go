package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
)

// Codes the reconciler records itself on a pool role link.
const (
	CodeVerifierNotConfigured = links.CodeVerifierNotConfigured
	CodeNotAllowed            = links.CodeNotAllowed
	maxErrorLen               = 300
)

// syncLinkEligibility applies the allowed-accounts filter to the pool's links before the role is built:
// narrowing the filter disables the links outside it (their profile and assume statement go with them) and
// widening it brings a disabled link back to pending_trust (RC-US-4.2). The pool's in-memory links are
// updated so this pass renders what the database now says.
func (c *Controller) syncLinkEligibility(ctx context.Context, d *Desired) error {
	var errs []error
	for i := range d.RoleLinks {
		l := &d.RoleLinks[i]
		base := LinkChange{TeamID: d.Team.ID, PoolID: d.ID, LinkID: l.ID, From: l.Status, AccountID: l.AccountID, RoleARN: l.RoleARN}
		switch {
		case !l.Allowed && l.Status != domain.RoleLinkStatusDisabled:
			base.To = domain.RoleLinkStatusDisabled
			base.Error = CodeNotAllowed + ": the account is not an active account in the team's allowed accounts"
			if err := c.recordLink(ctx, base); err != nil {
				errs = append(errs, fmt.Errorf("link %s: %w", l.ID, err))
				continue
			}
			l.Status = domain.RoleLinkStatusDisabled
		case l.Allowed && l.Status == domain.RoleLinkStatusDisabled:
			base.To = domain.RoleLinkStatusPendingTrust
			if err := c.recordLink(ctx, base); err != nil {
				errs = append(errs, fmt.Errorf("link %s: %w", l.ID, err))
				continue
			}
			l.Status, l.UpdatedAt = domain.RoleLinkStatusPendingTrust, c.cfg.Now().UTC()
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) recordLink(ctx context.Context, ch LinkChange) error {
	if c.cfg.Recorder == nil {
		return errors.New("pool: no recorder for role links")
	}
	return c.cfg.Recorder.RecordLink(ctx, ch)
}

// due reports whether the link is to be verified now, and when it next will be otherwise.
func due(l RoleLink, now time.Time) (bool, time.Duration) {
	switch l.Status {
	case domain.RoleLinkStatusPendingTrust:
		return true, 0
	case domain.RoleLinkStatusFailed:
		if wait := l.UpdatedAt.Add(links.FailedRetry).Sub(now); wait > 0 {
			return false, wait
		}
		return true, 0
	case domain.RoleLinkStatusVerified:
		at := l.UpdatedAt
		if l.LastVerified != nil {
			at = *l.LastVerified
		}
		if wait := at.Add(links.VerifyInterval).Sub(now); wait > 0 {
			return false, wait
		}
		return true, 0
	}
	return false, 0
}

// verifyLinks verifies each link that is due: it assumes the pool's own role and, with that session, the
// link's role with the external id and the pool id as the session name (D51). A transition into verified or
// failed publishes the contract event. The pool is not verified while its team is suspended. It returns when
// the next link is due, and RoleLinksVerified: True when every link that is not disabled is verified.
func (c *Controller) verifyLinks(ctx context.Context, d Desired) (time.Duration, []domain.Condition, error) {
	if len(d.RoleLinks) == 0 {
		return 0, nil, nil
	}
	now := c.cfg.Now().UTC()
	var (
		errs   []error
		next   time.Duration
		failed []string
		wait   []string
	)
	for _, l := range d.RoleLinks {
		w, status, err := c.verifyLink(ctx, d, l, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("link %s: %w", l.ID, err))
		}
		next = soonest(next, w)
		switch status {
		case domain.RoleLinkStatusFailed:
			failed = append(failed, l.AccountID)
		case domain.RoleLinkStatusPendingTrust:
			wait = append(wait, l.AccountID)
		}
	}
	cond := ok(CondRoleLinksVerified, "Verified")
	switch {
	case len(failed) > 0:
		cond = domain.Condition{Type: CondRoleLinksVerified, Status: domain.ConditionFalse, Reason: "VerificationFailed",
			Message: fmt.Sprintf("the role link failed in account(s) %v", failed)}
	case len(wait) > 0:
		cond = domain.Condition{Type: CondRoleLinksVerified, Status: domain.ConditionUnknown, Reason: "PendingTrust",
			Message: fmt.Sprintf("waiting to verify account(s) %v", wait)}
	}
	return next, []domain.Condition{cond}, errors.Join(errs...)
}

// verifyLink handles one link and returns when it next needs looking at (0: only on change) and the status
// it has afterwards.
func (c *Controller) verifyLink(ctx context.Context, d Desired, l RoleLink, now time.Time) (time.Duration, domain.RoleLinkStatus, error) {
	if l.Status == domain.RoleLinkStatusDisabled || d.Team.Suspended() {
		return 0, l.Status, nil
	}
	isDue, wait := due(l, now)
	if !isDue {
		return wait, l.Status, nil
	}
	ch := LinkChange{TeamID: d.Team.ID, PoolID: d.ID, LinkID: l.ID, From: l.Status, AccountID: l.AccountID, RoleARN: l.RoleARN, VerifiedAt: &now}
	if err := c.verify(ctx, d, l); err == nil {
		ch.To = domain.RoleLinkStatusVerified
		if l.Status != domain.RoleLinkStatusVerified {
			ch.Event = domain.EventRoleLinkVerified
		}
	} else {
		ch.To, ch.VerifiedAt, ch.Error = domain.RoleLinkStatusFailed, nil, describe(err)
		if l.Status != domain.RoleLinkStatusFailed {
			ch.Event = domain.EventRoleLinkFailed
		}
	}
	if err := c.recordLink(ctx, ch); err != nil {
		return 0, l.Status, err
	}
	if ch.To == domain.RoleLinkStatusVerified {
		return links.VerifyInterval, ch.To, nil
	}
	return links.FailedRetry, ch.To, nil
}

func (c *Controller) verify(ctx context.Context, d Desired, l RoleLink) error {
	if c.cfg.Probe == nil {
		return codeError(CodeVerifierNotConfigured)
	}
	roleARN, err := c.roleARNOf(d.Team.Namespace, d.ID)
	if err != nil {
		return err
	}
	max := l.MaxSessionSeconds
	if max == 0 {
		max = domain.DefaultRoleLinkSessionSeconds
	}
	// The probe tags the sessions afs-team-id and afs-agent-id; for a pool the second carries the pool id.
	return c.cfg.Probe.Verify(ctx, links.VerifyRequest{
		TeamID: d.Team.ID, AgentID: d.ID, AgentRoleARN: roleARN, TargetRoleARN: l.RoleARN,
		ExternalID: l.ExternalID, SessionName: d.ID, DurationSeconds: max,
	})
}

type codeError string

func (e codeError) Error() string     { return string(e) }
func (e codeError) ErrorCode() string { return string(e) }

// describe is the text recorded as a link's last error: the AWS error code, then the message, never longer
// than maxErrorLen.
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
