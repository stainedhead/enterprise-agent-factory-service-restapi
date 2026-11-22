package pool_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool/pooltest"
)

const (
	arn1 = "arn:aws:iam::444455556666:role/shared-reviewer"
	arn2 = "arn:aws:iam::777788889999:role/shared-reviewer"
)

func link(id, acct, arn string, status domain.RoleLinkStatus) pool.RoleLink {
	return pool.RoleLink{ID: id, AccountID: acct, RoleARN: arn, ProfileName: "acct-" + acct, ExternalID: "afs-0123456789abcdefghij" + id,
		MaxSessionSeconds: 3600, Status: status, Allowed: true, UpdatedAt: now.Add(-48 * time.Hour)}
}

func withLinks(ls ...pool.RoleLink) pool.Desired {
	d := desired(poolA, "fixer")
	d.RoleLinks = ls
	return d
}

func assumeTargets(t *testing.T, r *rig) []string {
	t.Helper()
	spec, ok := r.iam.Role("afs-01JD0000000000000000000031")
	if !ok {
		t.Fatal("pool role missing")
	}
	var doc struct {
		Statement []struct {
			Sid      string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(spec.Policies["pool"]), &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Statement {
		if s.Sid == links.SidAssumeLinks {
			return s.Resource
		}
	}
	return nil
}

// M5-C3: the pool's shared role is linked in each listed account: the pool role may assume exactly those
// roles, and each link is verified through the pool's own role with the pool id as the session name.
func TestRoleLinksVerifiedInEachAccount(t *testing.T) {
	r := newRig(t, pool.Hooks{}, withLinks(
		link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust),
		link("2", "777788889999", arn2, domain.RoleLinkStatusPendingTrust)))
	out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(assumeTargets(t, r), ","); got != arn1+","+arn2 {
		t.Errorf("assume statement = %q, want exactly the two linked roles", got)
	}
	if len(r.prb.Requests) != 2 {
		t.Fatalf("verifications = %d", len(r.prb.Requests))
	}
	for _, req := range r.prb.Requests {
		if req.AgentRoleARN != roleARN(poolA) || req.SessionName != poolA || req.TeamID != teamID || req.ExternalID == "" || req.DurationSeconds != 3600 {
			t.Errorf("request = %+v", req)
		}
	}
	if len(r.rec.Changes) != 2 {
		t.Fatalf("changes = %+v", r.rec.Changes)
	}
	for _, c := range r.rec.Changes {
		if c.To != domain.RoleLinkStatusVerified || c.From != domain.RoleLinkStatusPendingTrust || c.Event != domain.EventRoleLinkVerified || c.PoolID != poolA || c.VerifiedAt == nil {
			t.Errorf("change = %+v", c)
		}
	}
	if c, _ := cond(out, pool.CondRoleLinksVerified); c.Status != domain.ConditionTrue {
		t.Errorf("condition = %+v", c)
	}
	// The AWS config the pods get names both links.
	if got := r.cl.Last().Pool.RoleLinks; len(got) != 2 {
		t.Errorf("links in environment = %d", len(got))
	}
}

func TestRoleLinkFailureAndRetry(t *testing.T) {
	failing := pooltest.CodedError{Code: "AccessDenied", Msg: "not authorized to assume " + arn1}
	t.Run("failure is recorded with the AWS code and an event", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust)))
		r.prb.Fail[arn1] = failing
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil {
			t.Fatal(err)
		}
		c := r.rec.Changes[0]
		if c.To != domain.RoleLinkStatusFailed || c.Event != domain.EventRoleLinkFailed || !strings.HasPrefix(c.Error, "AccessDenied") || c.VerifiedAt != nil {
			t.Errorf("change = %+v", c)
		}
		if v, _ := cond(out, pool.CondRoleLinksVerified); v.Status != domain.ConditionFalse || v.Reason != "VerificationFailed" {
			t.Errorf("condition = %+v", v)
		}
		if out.Requeue != links.FailedRetry {
			t.Errorf("requeue = %v, want %v", out.Requeue, links.FailedRetry)
		}
	})
	t.Run("a failed link is retried after the retry delay only", func(t *testing.T) {
		l := link("1", "444455556666", arn1, domain.RoleLinkStatusFailed)
		l.UpdatedAt = now.Add(-10 * time.Minute)
		r := newRig(t, pool.Hooks{}, withLinks(l))
		out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
		if err != nil || len(r.prb.Requests) != 0 || len(r.rec.Changes) != 0 {
			t.Fatalf("err %v requests %d changes %d", err, len(r.prb.Requests), len(r.rec.Changes))
		}
		if out.Requeue != 50*time.Minute {
			t.Errorf("requeue = %v", out.Requeue)
		}
		l.UpdatedAt = now.Add(-2 * time.Hour)
		r = newRig(t, pool.Hooks{}, withLinks(l))
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil || len(r.rec.Changes) != 1 || r.rec.Changes[0].To != domain.RoleLinkStatusVerified {
			t.Fatalf("err %v changes %+v", err, r.rec.Changes)
		}
	})
	t.Run("a verified link is checked daily and sends no event when unchanged", func(t *testing.T) {
		l := link("1", "444455556666", arn1, domain.RoleLinkStatusVerified)
		recent := now.Add(-time.Hour)
		l.LastVerified = &recent
		r := newRig(t, pool.Hooks{}, withLinks(l))
		out, _ := r.ctl.Reconcile(t.Context(), ref(poolA))
		if len(r.prb.Requests) != 0 || out.Requeue != 23*time.Hour {
			t.Errorf("requests %d requeue %v", len(r.prb.Requests), out.Requeue)
		}
		old := now.Add(-25 * time.Hour)
		l.LastVerified = &old
		r = newRig(t, pool.Hooks{}, withLinks(l))
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil || len(r.rec.Changes) != 1 || r.rec.Changes[0].Event != "" {
			t.Fatalf("err %v changes %+v", err, r.rec.Changes)
		}
	})
	t.Run("no probe records VerifierNotConfigured", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust)))
		ctl, err := pool.NewController(pool.Config{Source: r.src, Cluster: r.cl, IAM: r.iam, PodIdentity: r.pi, Queues: r.q, Recorder: r.rec, AWS: awsCfg(), Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
			t.Fatal(err)
		}
		if c := r.rec.Changes[0]; c.To != domain.RoleLinkStatusFailed || !strings.HasPrefix(c.Error, pool.CodeVerifierNotConfigured) {
			t.Errorf("change = %+v", c)
		}
	})
	t.Run("a recorder failure is returned", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust)))
		r.rec.Err = errors.New("db boom")
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err == nil {
			t.Fatal("error swallowed")
		}
	})
}

// RC-US-4.2 for pools: narrowing the allowed accounts disables the link and takes it out of the role and
// the profiles in the same pass; widening brings it back through verification.
func TestRoleLinkAllowedAccounts(t *testing.T) {
	t.Run("narrowed", func(t *testing.T) {
		l := link("1", "444455556666", arn1, domain.RoleLinkStatusVerified)
		l.Allowed = false
		l2 := link("2", "777788889999", arn2, domain.RoleLinkStatusVerified)
		recent := now.Add(-time.Hour)
		l2.LastVerified = &recent
		r := newRig(t, pool.Hooks{}, withLinks(l, l2))
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
			t.Fatal(err)
		}
		if len(r.rec.Changes) != 1 || r.rec.Changes[0].To != domain.RoleLinkStatusDisabled || !strings.HasPrefix(r.rec.Changes[0].Error, pool.CodeNotAllowed) {
			t.Fatalf("changes = %+v", r.rec.Changes)
		}
		if got := assumeTargets(t, r); len(got) != 1 || got[0] != arn2 {
			t.Errorf("assume statement = %v; the disabled link must be gone", got)
		}
		if st := r.cl.Last().Pool.RoleLinks[0].Status; st != domain.RoleLinkStatusDisabled {
			t.Errorf("rendered status = %s", st)
		}
	})
	t.Run("already disabled and still not allowed: nothing to do", func(t *testing.T) {
		l := link("1", "444455556666", arn1, domain.RoleLinkStatusDisabled)
		l.Allowed = false
		r := newRig(t, pool.Hooks{}, withLinks(l))
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil || len(r.rec.Changes) != 0 || len(r.prb.Requests) != 0 {
			t.Fatalf("err %v changes %+v", err, r.rec.Changes)
		}
		if got := assumeTargets(t, r); len(got) != 0 {
			t.Errorf("assume statement = %v", got)
		}
	})
	t.Run("widened", func(t *testing.T) {
		r := newRig(t, pool.Hooks{}, withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusDisabled)))
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
			t.Fatal(err)
		}
		if len(r.rec.Changes) != 2 || r.rec.Changes[0].To != domain.RoleLinkStatusPendingTrust || r.rec.Changes[1].From != domain.RoleLinkStatusPendingTrust || r.rec.Changes[1].To != domain.RoleLinkStatusVerified {
			t.Fatalf("changes = %+v", r.rec.Changes)
		}
	})
	t.Run("a team that is suspended verifies nothing", func(t *testing.T) {
		d := withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust))
		d.Team.State = "suspended"
		r := newRig(t, pool.Hooks{}, d)
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil || len(r.prb.Requests) != 0 {
			t.Fatalf("err %v requests %d", err, len(r.prb.Requests))
		}
	})
}

func TestLinkVerificationNeedsTheRole(t *testing.T) {
	r := newRig(t, pool.Hooks{}, withLinks(link("1", "444455556666", arn1, domain.RoleLinkStatusPendingTrust)))
	r.iam.Err = errors.New("iam boom")
	if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err == nil {
		t.Fatal("no error")
	}
	if len(r.prb.Requests) != 0 {
		t.Error("a link was verified through a role that does not exist")
	}
}
