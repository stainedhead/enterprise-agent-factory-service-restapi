package links

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type src struct{ d Desired }

func (s *src) LoadLinks(context.Context, string) (Desired, error) { return s.d, nil }

type rec struct{ got []Change }

func (r *rec) Record(_ context.Context, c Change) error { r.got = append(r.got, c); return nil }

type probe struct {
	err   error
	calls []VerifyRequest
}

func (p *probe) Verify(_ context.Context, r VerifyRequest) error {
	p.calls = append(p.calls, r)
	return p.err
}

type awsErr struct{ code, msg string }

func (e awsErr) Error() string     { return e.msg }
func (e awsErr) ErrorCode() string { return e.code }

func link(status domain.RoleLinkStatus) Link {
	return Link{ID: "arl_1", AgentID: "agt_1", AccountID: "111122223333", RoleARN: "arn:aws:iam::111122223333:role/d",
		ExternalID: "afs-x", Status: status, Allowed: true, AgentDedicated: true, AgentLive: true, Namespace: "afs-t-pay",
		UpdatedAt: t0.Add(-time.Minute)}
}

func run(t *testing.T, state string, l Link, p Probe) (*rec, engine.Outcome, error) {
	t.Helper()
	r := &rec{}
	c, err := NewController(Config{Source: &src{Desired{TeamID: "team_1", State: state, Links: []Link{l}}}, Recorder: r, Probe: p,
		AgentRoleARN: func(ns, id string) string { return "arn:aws:iam::999988887777:role/afs/" + ns + "/" + id },
		Now:          func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Reconcile(t.Context(), engine.NewRef(Kind, "team_1", "team_1"))
	return r, out, err
}

func TestPendingTrustBecomesVerified(t *testing.T) {
	p := &probe{}
	r, out, err := run(t, "active", link(domain.RoleLinkStatusPendingTrust), p)
	if err != nil || len(r.got) != 1 {
		t.Fatalf("%v %+v", err, r.got)
	}
	c := r.got[0]
	if c.To != domain.RoleLinkStatusVerified || c.Event != domain.EventRoleLinkVerified || c.VerifiedAt == nil {
		t.Fatalf("%+v", c)
	}
	q := p.calls[0]
	if q.SessionName != "agt_1" || q.ExternalID != "afs-x" || q.TargetRoleARN != "arn:aws:iam::111122223333:role/d" ||
		q.AgentRoleARN != "arn:aws:iam::999988887777:role/afs/afs-t-pay/agt_1" || q.DurationSeconds != 3600 || q.TeamID != "team_1" {
		t.Fatalf("request: %+v", q)
	}
	if out.Requeue != VerifyInterval || !out.NoStatus {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestFailureRecordsTheAWSErrorCode(t *testing.T) {
	r, out, err := run(t, "active", link(domain.RoleLinkStatusPendingTrust), &probe{err: awsErr{"AccessDenied", "User is not authorized to perform: sts:AssumeRole"}})
	if err != nil || len(r.got) != 1 {
		t.Fatalf("%v %+v", err, r.got)
	}
	c := r.got[0]
	if c.To != domain.RoleLinkStatusFailed || c.Event != domain.EventRoleLinkFailed || !strings.HasPrefix(c.Error, "AccessDenied") || c.VerifiedAt != nil {
		t.Fatalf("%+v", c)
	}
	if out.Requeue != FailedRetry {
		t.Fatalf("requeue %v", out.Requeue)
	}
	long := strings.Repeat("x", 1000)
	r, _, _ = run(t, "active", link(domain.RoleLinkStatusPendingTrust), &probe{err: errors.New(long)})
	if len(r.got[0].Error) != maxErrorLen {
		t.Fatalf("error length %d", len(r.got[0].Error))
	}
}

func TestSchedule(t *testing.T) {
	old := func(s domain.RoleLinkStatus, ago time.Duration) Link {
		l := link(s)
		l.UpdatedAt = t0.Add(-ago)
		v := t0.Add(-ago)
		l.LastVerified = &v
		return l
	}
	for _, tc := range []struct {
		name   string
		l      Link
		verify bool
		event  domain.EventType
	}{
		{"verified, fresh", old(domain.RoleLinkStatusVerified, time.Hour), false, ""},
		{"verified, a day old: daily check, no event on an unchanged status", old(domain.RoleLinkStatusVerified, 25*time.Hour), true, ""},
		{"failed, just failed", old(domain.RoleLinkStatusFailed, time.Minute), false, ""},
		{"failed, retry due: success is a transition", old(domain.RoleLinkStatusFailed, 2*time.Hour), true, domain.EventRoleLinkVerified},
	} {
		p := &probe{}
		r, out, err := run(t, "active", tc.l, p)
		if err != nil || (len(p.calls) == 1) != tc.verify {
			t.Errorf("%s: calls=%d err=%v", tc.name, len(p.calls), err)
			continue
		}
		if tc.verify && (len(r.got) != 1 || r.got[0].Event != tc.event) {
			t.Errorf("%s: %+v", tc.name, r.got)
		}
		if !tc.verify && (len(r.got) != 0 || out.Requeue <= 0) {
			t.Errorf("%s: wrote %+v requeue %v", tc.name, r.got, out.Requeue)
		}
	}
}

func TestNarrowedFilterDisablesAndWidenedFilterEnables(t *testing.T) {
	l := link(domain.RoleLinkStatusVerified)
	l.Allowed = false
	p := &probe{}
	r, _, err := run(t, "active", l, p)
	if err != nil || len(r.got) != 1 || r.got[0].To != domain.RoleLinkStatusDisabled || !strings.HasPrefix(r.got[0].Error, CodeNotAllowed) || len(p.calls) != 0 {
		t.Fatalf("disable: %v %+v calls=%d", err, r.got, len(p.calls))
	}
	l.Status = domain.RoleLinkStatusDisabled
	if r, _, _ = run(t, "active", l, p); len(r.got) != 0 {
		t.Fatalf("already disabled must write nothing: %+v", r.got)
	}
	l.Allowed = true
	r, _, err = run(t, "active", l, p)
	if err != nil || len(r.got) != 2 || r.got[0].To != domain.RoleLinkStatusPendingTrust || r.got[1].To != domain.RoleLinkStatusVerified {
		t.Fatalf("enable: %v %+v", err, r.got)
	}
}

func TestNothingToVerify(t *testing.T) {
	shared := link(domain.RoleLinkStatusPendingTrust)
	shared.AgentDedicated = false
	gone := link(domain.RoleLinkStatusPendingTrust)
	gone.AgentLive = false
	for name, tc := range map[string]struct {
		state string
		l     Link
	}{"shared agent": {"active", shared}, "agent deleting": {"active", gone}, "team suspended": {"suspended", link(domain.RoleLinkStatusPendingTrust)},
		"team deleting": {"deleting", link(domain.RoleLinkStatusPendingTrust)}} {
		p := &probe{}
		r, _, err := run(t, tc.state, tc.l, p)
		if err != nil || len(p.calls) != 0 || len(r.got) != 0 {
			t.Errorf("%s: %v calls=%d wrote=%d", name, err, len(p.calls), len(r.got))
		}
	}
}

func TestNoProbeFailsClosed(t *testing.T) {
	r, _, err := run(t, "active", link(domain.RoleLinkStatusPendingTrust), nil)
	if err != nil || len(r.got) != 1 || r.got[0].To != domain.RoleLinkStatusFailed || !strings.HasPrefix(r.got[0].Error, CodeVerifierNotConfigured) {
		t.Fatalf("%v %+v", err, r.got)
	}
}

func TestRecorderFailureIsRetried(t *testing.T) {
	c, _ := NewController(Config{Source: &src{Desired{TeamID: "t", State: "active", Links: []Link{link(domain.RoleLinkStatusPendingTrust)}}},
		Recorder: failRec{}, Probe: &probe{}, AgentRoleARN: func(string, string) string { return "x" }, Now: func() time.Time { return t0 }})
	if _, err := c.Reconcile(t.Context(), engine.NewRef(Kind, "t", "t")); err == nil {
		t.Fatal("a failed write is an error for the engine's backoff")
	}
	if _, err := NewController(Config{}); err == nil {
		t.Fatal("config")
	}
}

type failRec struct{}

func (failRec) Record(context.Context, Change) error { return errors.New("db down") }

func TestRoutes(t *testing.T) {
	if _, err := engine.NewRegistry(Routes()...); err != nil {
		t.Fatal(err)
	}
}

func TestPolicies(t *testing.T) {
	base := `{"Version":"2012-10-17","Statement":[{"Sid":"X","Effect":"Allow","Action":["a:b"]}]}`
	if got, _ := WithAssumeStatements(base, nil); got != base {
		t.Fatal("no links, no statement")
	}
	got, err := WithAssumeStatements(base, []string{"arn:aws:iam::2:role/b", "arn:aws:iam::1:role/a", "arn:aws:iam::2:role/b"})
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Statement []struct {
			Sid      string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(got), &p); err != nil || len(p.Statement) != 2 || p.Statement[1].Sid != SidAssumeLinks ||
		len(p.Statement[1].Resource) != 2 || p.Statement[1].Resource[0] != "arn:aws:iam::1:role/a" || strings.Contains(got, "*") {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := WithAssumeStatements(got, []string{"arn:aws:iam::3:role/c"}); err == nil {
		t.Fatal("duplicate statement")
	}
	if _, err := WithAssumeStatements("nope", []string{"x"}); err == nil {
		t.Fatal("not JSON")
	}
	tr, err := TrustWithVerifier(base, "arn:aws:iam::9:role/reconciler")
	if err != nil || !strings.Contains(tr, SidVerifier) || !strings.Contains(tr, "sts:TagSession") {
		t.Fatalf("%s %v", tr, err)
	}
	if same, _ := TrustWithVerifier(base, ""); same != base {
		t.Fatal("no verifier")
	}
}
