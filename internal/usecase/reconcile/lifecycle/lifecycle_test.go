package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// log records every call in order across all fakes.
type log struct{ calls []string }

func (l *log) add(s string) { l.calls = append(l.calls, s) }

type fakeStore struct {
	conds  []domain.Condition
	phase  domain.Phase
	writes int
}

func (s *fakeStore) Conditions(context.Context, string, string) ([]domain.Condition, error) {
	return slices.Clone(s.conds), nil
}
func (s *fakeStore) Write(_ context.Context, _ Target, p domain.Phase, c []domain.Condition) error {
	s.phase, s.conds = p, slices.Clone(c)
	s.writes++
	return nil
}
func (s *fakeStore) cond(t string) domain.Condition {
	for _, c := range s.conds {
		if c.Type == t {
			return c
		}
	}
	return domain.Condition{}
}

type fakeCompleter struct {
	l   *log
	n   int
	err error
}

func (c *fakeCompleter) Complete(context.Context, Target) error {
	c.n++
	c.l.add("complete")
	return c.err
}

type coded struct{ code string }

func (c coded) Error() string { return "aws: " + c.code }
func (c coded) Code() string  { return c.code }

type fakeAWS struct {
	l          *log
	secrets    []string
	listErr    error
	schedErr   map[string]error
	scheduled  map[string]int
	revokeErr  error
	profErr    error
	accessErr  error
	nsGoneAt   int // namespace is gone from this call on
	nsCalls    int
	nsErr      error
	revokedFor []string
}

func (f *fakeAWS) ListTeamSecrets(context.Context, string) ([]string, error) {
	return f.secrets, f.listErr
}
func (f *fakeAWS) ScheduleDeletion(_ context.Context, arn string, days int) error {
	f.l.add("secret:" + arn)
	if err := f.schedErr[arn]; err != nil {
		return err
	}
	if f.scheduled == nil {
		f.scheduled = map[string]int{}
	}
	f.scheduled[arn] = days
	return nil
}
func (f *fakeAWS) RevokeTeam(_ context.Context, id string) error {
	f.l.add("iam")
	f.revokedFor = append(f.revokedFor, id)
	return f.revokeErr
}
func (f *fakeAWS) DeleteTeamProfiles(context.Context, string) error {
	f.l.add("profiles")
	return f.profErr
}
func (f *fakeAWS) DeleteTeamAccessEntries(context.Context, string) error {
	f.l.add("access")
	return f.accessErr
}
func (f *fakeAWS) DeleteNamespace(_ context.Context, name string) (bool, error) {
	f.nsCalls++
	f.l.add("namespace:" + name)
	return f.nsCalls >= f.nsGoneAt, f.nsErr
}

func newFixture() (*Finalizer, *fakeAWS, *fakeStore, *fakeCompleter, *log) {
	l := &log{}
	a := &fakeAWS{l: l, secrets: []string{"arn:aws:secretsmanager:us-east-1:111122223333:secret:b", "arn:aws:secretsmanager:us-east-1:111122223333:secret:a"}, nsGoneAt: 1}
	st, c := &fakeStore{}, &fakeCompleter{l: l}
	hooks := Hooks{
		Agents: []Step{StepFunc{"Agents", func(context.Context, Target) error { l.add("agents"); return nil }}},
		Pools:  []Step{StepFunc{"Pools", func(context.Context, Target) error { l.add("pools"); return nil }}},
	}
	f := NewTeamFinalizer(TeamDeps{Secrets: a, IAM: a, Profiles: a, Access: a, NS: a}, hooks, st, c)
	f.Now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	return f, a, st, c, l
}

var team = Target{Kind: "team", ID: "team_01", TeamID: "team_01", Slug: "alpha", Namespace: "afs-t-alpha"}

func TestTeamFinalizerOrderAndCompletion(t *testing.T) {
	f, a, st, c, l := newFixture()
	res, err := f.Reconcile(context.Background(), team)
	if err != nil || !res.Done {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := []string{"agents", "pools",
		"secret:arn:aws:secretsmanager:us-east-1:111122223333:secret:b", "secret:arn:aws:secretsmanager:us-east-1:111122223333:secret:a",
		"iam", "profiles", "access", "namespace:afs-t-alpha", "complete"}
	// secrets are scheduled in sorted order
	want[2], want[3] = want[3], want[2]
	if !slices.Equal(l.calls, want) {
		t.Fatalf("order\n got %v\nwant %v", l.calls, want)
	}
	for arn, days := range a.scheduled {
		if days != 30 {
			t.Errorf("%s recovery window = %d, want 30", arn, days)
		}
	}
	if st.phase != domain.PhaseDeleting {
		t.Errorf("phase = %s", st.phase)
	}
	for _, s := range f.Steps {
		if got := st.cond(CondType(s.Name())); got.Status != domain.ConditionTrue {
			t.Errorf("condition %s = %+v", CondType(s.Name()), got)
		}
	}
	if c.n != 1 {
		t.Errorf("completed %d times", c.n)
	}
}

func TestSecretsScheduledBeforeNamespace(t *testing.T) {
	_, _, _, _, l := func() (*Finalizer, *fakeAWS, *fakeStore, *fakeCompleter, *log) {
		f, a, s, c, l := newFixture()
		if _, err := f.Reconcile(context.Background(), team); err != nil {
			t.Fatal(err)
		}
		return f, a, s, c, l
	}()
	ns := slices.IndexFunc(l.calls, func(s string) bool { return strings.HasPrefix(s, "namespace:") })
	for i, c := range l.calls {
		if strings.HasPrefix(c, "secret:") && i > ns {
			t.Fatalf("secret scheduled after the namespace: %v", l.calls)
		}
	}
}

func TestFailedStepRetriedNextPass(t *testing.T) {
	tests := []struct {
		name       string
		break_     func(a *fakeAWS)
		fix        func(a *fakeAWS)
		condType   string
		wantReason string
		after      []string // calls that must not have happened in the failing pass
	}{
		{"iam", func(a *fakeAWS) { a.revokeErr = coded{"AccessDenied"} }, func(a *fakeAWS) { a.revokeErr = nil }, CondType(StepRevokeIAM), "AccessDenied", []string{"profiles", "access", "complete"}},
		{"profiles", func(a *fakeAWS) { a.profErr = errors.New("boom") }, func(a *fakeAWS) { a.profErr = nil }, CondType(StepDeleteInferenceProfile), "Error", []string{"access", "complete"}},
		{"access entry", func(a *fakeAWS) { a.accessErr = coded{"ThrottlingException"} }, func(a *fakeAWS) { a.accessErr = nil }, CondType(StepDeleteAccessEntry), "ThrottlingException", []string{"complete"}},
		{"list secrets", func(a *fakeAWS) { a.listErr = coded{"InternalServiceError"} }, func(a *fakeAWS) { a.listErr = nil }, CondType(StepScheduleSecretDeletion), "InternalServiceError", []string{"iam", "complete"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, a, st, c, l := newFixture()
			tt.break_(a)
			res, err := f.Reconcile(context.Background(), team)
			if err == nil || res.Done {
				t.Fatalf("want failure, res=%+v err=%v", res, err)
			}
			got := st.cond(tt.condType)
			if got.Status != domain.ConditionFalse || got.Reason != tt.wantReason {
				t.Errorf("condition = %+v", got)
			}
			for _, bad := range tt.after {
				if slices.Contains(l.calls, bad) {
					t.Errorf("%q ran after a failed step", bad)
				}
			}
			if st.phase != domain.PhaseDeleting || c.n != 0 {
				t.Errorf("phase=%s completed=%d", st.phase, c.n)
			}
			// next pass: fixed; the steps that already succeeded are not run again
			tt.fix(a)
			before := len(l.calls)
			res, err = f.Reconcile(context.Background(), team)
			if err != nil || !res.Done {
				t.Fatalf("second pass res=%+v err=%v", res, err)
			}
			for _, c := range l.calls[before:] {
				if c == "agents" || c == "pools" {
					t.Errorf("hook %s re-run after it succeeded", c)
				}
			}
		})
	}
}

func TestOneBadSecretDoesNotHoldTheOthers(t *testing.T) {
	f, a, _, _, _ := newFixture()
	a.schedErr = map[string]error{"arn:aws:secretsmanager:us-east-1:111122223333:secret:a": coded{"InvalidRequestException"}}
	if _, err := f.Reconcile(context.Background(), team); err == nil {
		t.Fatal("want error")
	}
	if a.scheduled["arn:aws:secretsmanager:us-east-1:111122223333:secret:b"] != 30 {
		t.Error("the healthy secret was not scheduled")
	}
}

func TestPendingNamespaceRequeuesWithoutError(t *testing.T) {
	f, a, st, c, _ := newFixture()
	a.nsGoneAt = 3
	for i := 0; i < 2; i++ {
		res, err := f.Reconcile(context.Background(), team)
		if err != nil || res.Done || res.RequeueAfter != PendingRequeue {
			t.Fatalf("pass %d: res=%+v err=%v", i, res, err)
		}
		got := st.cond(CondType(StepDeleteNamespace))
		if got.Status != domain.ConditionFalse || got.Reason != "InProgress" {
			t.Errorf("condition = %+v", got)
		}
	}
	if c.n != 0 {
		t.Fatal("completed while the namespace was terminating")
	}
	res, err := f.Reconcile(context.Background(), team)
	if err != nil || !res.Done || c.n != 1 {
		t.Fatalf("res=%+v err=%v completed=%d", res, err, c.n)
	}
}

func TestNamespaceErrors(t *testing.T) {
	f, a, _, _, _ := newFixture()
	a.nsErr = coded{"Forbidden"}
	if _, err := f.Reconcile(context.Background(), team); err == nil {
		t.Fatal("want error")
	}
	f2, _, _, _, _ := newFixture()
	noNS := team
	noNS.Namespace = ""
	if _, err := f2.Reconcile(context.Background(), noNS); err == nil {
		t.Fatal("want error for a team without a namespace")
	}
}

func TestCompleteFailureRetries(t *testing.T) {
	f, _, _, c, _ := newFixture()
	c.err = errors.New("db down")
	if res, err := f.Reconcile(context.Background(), team); err == nil || res.Done {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	c.err = nil
	if res, err := f.Reconcile(context.Background(), team); err != nil || !res.Done {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestConditionTransitionTimeKeptWhenUnchanged(t *testing.T) {
	f, a, st, _, _ := newFixture()
	a.nsGoneAt = 99
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return t0 }
	_, _ = f.Reconcile(context.Background(), team)
	first := st.cond(CondType(StepDeleteNamespace)).LastTransitionTime
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	_, _ = f.Reconcile(context.Background(), team)
	if got := st.cond(CondType(StepDeleteNamespace)).LastTransitionTime; !got.Equal(first) {
		t.Errorf("transition time moved from %v to %v with no change", first, got)
	}
}

func TestStoreErrors(t *testing.T) {
	f, _, _, _, _ := newFixture()
	f.Store = errStore{}
	if _, err := f.Reconcile(context.Background(), team); err == nil {
		t.Fatal("want error")
	}
}

type errStore struct{}

func (errStore) Conditions(context.Context, string, string) ([]domain.Condition, error) {
	return nil, errors.New("down")
}
func (errStore) Write(context.Context, Target, domain.Phase, []domain.Condition) error {
	return errors.New("down")
}

func TestUnionSecrets(t *testing.T) {
	l := &log{}
	a := &fakeAWS{l: l, secrets: []string{"x", "y"}}
	b := &fakeAWS{l: l, secrets: []string{"y", "z"}}
	u := UnionSecrets{Sources: []interface {
		ListTeamSecrets(ctx context.Context, teamID string) ([]string, error)
	}{a, b}, Scheduler: a}
	got, err := u.ListTeamSecrets(context.Background(), "team_01")
	if err != nil || !slices.Equal(got, []string{"x", "y", "z"}) {
		t.Fatalf("got %v err %v", got, err)
	}
	if err := u.ScheduleDeletion(context.Background(), "x", 30); err != nil || a.scheduled["x"] != 30 {
		t.Fatal("not scheduled")
	}
	b.listErr = errors.New("x")
	if _, err := u.ListTeamSecrets(context.Background(), "t"); err == nil {
		t.Fatal("want error")
	}
}

// ---- suspend and resume ----

type memPrior struct {
	m     map[string]map[string]json.RawMessage
	saves int
}

func (p *memPrior) Save(_ context.Context, id string, s map[string]json.RawMessage) error {
	if p.m == nil {
		p.m = map[string]map[string]json.RawMessage{}
	}
	cp := map[string]json.RawMessage{}
	for k, v := range s {
		cp[k] = v
	}
	p.m[id] = cp
	p.saves++
	return nil
}
func (p *memPrior) Load(_ context.Context, id string) (map[string]json.RawMessage, error) {
	return p.m[id], nil
}
func (p *memPrior) Clear(_ context.Context, id string) error { delete(p.m, id); return nil }

type fakeHook struct {
	name     string
	live     []WorkloadState
	suspErr  error
	resumeEr error
	resumed  []WorkloadState
}

func (h *fakeHook) Name() string { return h.name }
func (h *fakeHook) Suspend(context.Context, string) ([]WorkloadState, error) {
	if h.suspErr != nil {
		return nil, h.suspErr
	}
	out := h.live
	h.live = nil // scaled to zero
	return out, nil
}
func (h *fakeHook) Resume(_ context.Context, _ string, prior []WorkloadState) error {
	h.resumed = prior
	return h.resumeEr
}

func TestSuspendRecordsPriorStateAndResumeRestoresIt(t *testing.T) {
	agents := &fakeHook{name: "Agents", live: []WorkloadState{{Kind: "agent", ID: "agt_1", Replicas: 1}, {Kind: "agent", ID: "agt_2", Replicas: 0, Paused: true}}}
	pools := &fakeHook{name: "Pools", live: []WorkloadState{{Kind: "pool", ID: "pool_1", Replicas: 4}}}
	st := &memPrior{}
	s := &SuspendResume{Hooks: []ScaleHook{pools, agents}, Store: st}
	ctx := context.Background()
	if err := s.Suspend(ctx, "team_01"); err != nil {
		t.Fatal(err)
	}
	// a second pass finds the workloads already at zero and must keep the first record
	if err := s.Suspend(ctx, "team_01"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx, "team_01"); err != nil {
		t.Fatal(err)
	}
	if len(agents.resumed) != 2 || !agents.resumed[1].Paused || agents.resumed[0].Replicas != 1 {
		t.Errorf("agents resumed with %+v", agents.resumed)
	}
	if len(pools.resumed) != 1 || pools.resumed[0].Replicas != 4 {
		t.Errorf("pools resumed with %+v", pools.resumed)
	}
	if _, ok := st.m["team_01"]; ok {
		t.Error("prior state not cleared after resume")
	}
}

func TestSuspendHookFailureKeepsOtherHooksWork(t *testing.T) {
	agents := &fakeHook{name: "Agents", suspErr: errors.New("api down")}
	pools := &fakeHook{name: "Pools", live: []WorkloadState{{Kind: "pool", ID: "p", Replicas: 2}}}
	st := &memPrior{}
	s := &SuspendResume{Hooks: []ScaleHook{agents, pools}, Store: st}
	if err := s.Suspend(context.Background(), "team_01"); err == nil {
		t.Fatal("want error")
	}
	if _, ok := st.m["team_01"]["Pools"]; !ok {
		t.Error("pool state not recorded though its hook succeeded")
	}
}

func TestResumeFailureKeepsRecord(t *testing.T) {
	h := &fakeHook{name: "Agents", live: []WorkloadState{{Kind: "agent", ID: "a", Replicas: 1}}}
	st := &memPrior{}
	s := &SuspendResume{Hooks: []ScaleHook{h}, Store: st}
	ctx := context.Background()
	_ = s.Suspend(ctx, "t")
	h.resumeEr = errors.New("nope")
	if err := s.Resume(ctx, "t"); err == nil {
		t.Fatal("want error")
	}
	if _, ok := st.m["t"]; !ok {
		t.Fatal("record dropped though resume failed")
	}
	h.resumeEr = nil
	if err := s.Resume(ctx, "t"); err != nil || len(h.resumed) != 1 {
		t.Fatalf("retry: %v %+v", err, h.resumed)
	}
}

func TestResumeWithNothingRecorded(t *testing.T) {
	h := &fakeHook{name: "Agents"}
	s := &SuspendResume{Hooks: []ScaleHook{h}, Store: &memPrior{}}
	if err := s.Resume(context.Background(), "t"); err != nil || h.resumed != nil {
		t.Fatalf("err=%v resumed=%v", err, h.resumed)
	}
}

// FR-R13: a failing delete step writes a sanitised message into the step's condition; the returned
// error keeps the full text for the log.
func TestStepConditionMessageIsSanitised(t *testing.T) {
	f, a, st, _, _ := newFixture()
	a.revokeErr = errors.New("AccessDenied: arn:aws:sts::111122223333:assumed-role/afs-reconciler/s cannot iam:DeleteRole " + strings.Repeat("x", 800))
	_, err := f.Reconcile(context.Background(), team)
	if err == nil || !strings.Contains(err.Error(), "111122223333") {
		t.Fatalf("want the full error returned for the log, got %v", err)
	}
	got := st.cond(CondType(StepRevokeIAM))
	if strings.Contains(got.Message, "111122223333") || strings.Contains(got.Message, "arn:aws") || len(got.Message) > 512 {
		t.Errorf("message not sanitised: %q", got.Message)
	}
	if !strings.Contains(got.Message, "AccessDenied") {
		t.Errorf("message lost its meaning: %q", got.Message)
	}
}
