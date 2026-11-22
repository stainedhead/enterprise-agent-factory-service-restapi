package logs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

const teamID = "team_01JD0000000000000000000011"

type src struct {
	d   logs.Desired
	err error
}

func (s *src) LoadLogs(context.Context, string) (logs.Desired, error) { return s.d, s.err }

type cluster struct {
	applied   []logs.Spec
	removed   []string
	applyErr  error
	removeErr error
}

func (c *cluster) ApplyAggregator(_ context.Context, s logs.Spec) (logs.ApplyResult, error) {
	c.applied = append(c.applied, s)
	return logs.ApplyResult{Changed: []string{"Deployment/x"}}, c.applyErr
}

func (c *cluster) RemoveAggregator(_ context.Context, ns, _ string) (logs.ApplyResult, error) {
	c.removed = append(c.removed, ns)
	return logs.ApplyResult{}, c.removeErr
}

type statuses struct{ reports []engine.Report }

func (s *statuses) Apply(_ context.Context, r engine.Report) error {
	s.reports = append(s.reports, r)
	return nil
}

var master = []byte("master-key-0123456789")

func cw(id, name string, enabled bool) logs.Destination {
	return logs.Destination{ID: id, Name: name, Kind: domain.LogCloudWatch, Enabled: enabled, Version: 3,
		Config: map[string]string{"logGroup": "/afs/x", "region": "us-east-1"}}
}

func hec(id string) logs.Destination {
	return logs.Destination{ID: id, Name: "hec", Kind: domain.LogSplunkHEC, Enabled: true, Version: 1, SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/t/hec-AbCdEf", //nolint:gosec // a fixture ARN
		Config: map[string]string{"endpoint": "https://splunk.example.com:8088"}}
}

func newCtl(t *testing.T, s *src, c *cluster, st *statuses) *logs.Controller {
	t.Helper()
	cfg := logs.Config{Source: s, Cluster: c, MasterKey: master, Now: func() time.Time { return time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC) }}
	if st != nil {
		cfg.DestinationStatus = st
	}
	ctl, err := logs.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ctl
}

func cond(o engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range o.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func TestApplyEnabledDestinations(t *testing.T) {
	s := &src{d: logs.Desired{TeamID: teamID, Slug: "a", Namespace: "afs-t-a", State: "active",
		Destinations: []logs.Destination{hec("log_B"), cw("log_A", "cw", true), cw("log_C", "off", false)}}}
	c, st := &cluster{}, &statuses{}
	out, err := newCtl(t, s, c, st).Reconcile(t.Context(), engine.NewRef(logs.Kind, teamID, teamID))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.applied) != 1 || len(c.applied[0].Outputs) != 2 || len(c.removed) != 0 {
		t.Fatalf("applied=%d removed=%v", len(c.applied), c.removed)
	}
	for _, o := range c.applied[0].Outputs {
		if o.ID == "log_C" {
			t.Fatal("a disabled destination became an output")
		}
	}
	if len(c.applied[0].RedactionKey) != 32 {
		t.Fatalf("redaction key length %d", len(c.applied[0].RedactionKey))
	}
	if cd, ok := cond(out, logs.CondAggregatorReady); !ok || cd.Status != domain.ConditionTrue {
		t.Fatalf("conditions %+v", out.Conditions)
	}
	if len(out.Changed) != 1 {
		t.Fatalf("changed %v", out.Changed)
	}
	// Every destination gets its own status row: output applied, or Unknown when disabled.
	got := map[string]domain.ConditionStatus{}
	for _, r := range st.reports {
		if string(r.Kind) != logs.StatusKindDestination || r.TeamID != teamID || !r.Replace || *r.Generation == 0 {
			t.Fatalf("report %+v", r)
		}
		got[r.ID] = r.Conditions[0].Status
	}
	if got["log_A"] != domain.ConditionTrue || got["log_B"] != domain.ConditionTrue || got["log_C"] != domain.ConditionUnknown {
		t.Fatalf("destination statuses %v", got)
	}
}

// RC-US-16.4: removing the last enabled destination removes the aggregator.
func TestNoEnabledDestinationRemovesTheAggregator(t *testing.T) {
	s := &src{d: logs.Desired{TeamID: teamID, Slug: "a", Namespace: "afs-t-a", State: "active", Destinations: []logs.Destination{cw("log_A", "cw", false)}}}
	c := &cluster{}
	out, err := newCtl(t, s, c, nil).Reconcile(t.Context(), engine.NewRef(logs.Kind, teamID, teamID))
	if err != nil || len(c.applied) != 0 || len(c.removed) != 1 || c.removed[0] != "afs-t-a" {
		t.Fatalf("applied=%d removed=%v err=%v", len(c.applied), c.removed, err)
	}
	if cd, _ := cond(out, logs.CondAggregatorReady); cd.Reason != "NoDestinations" {
		t.Fatalf("%+v", out.Conditions)
	}
}

// The reconciler guards what it renders: an invalid row (written around the API) is refused, the
// valid destinations still get their outputs, and the destination reports why.
func TestInvalidDestinationIsRefusedOthersApply(t *testing.T) {
	bad := cw("log_BAD", "bad", true)
	bad.Config["logGroup"] = "g\nInjected = yes"
	withToken := hec("log_TOK")
	withToken.Config["splunkToken"] = "abc"
	envRef := cw("log_ENV", "env", true)
	envRef.Config["logGroup"] = "${AFS_LOG_OTHER}"
	s := &src{d: logs.Desired{TeamID: teamID, Slug: "a", Namespace: "afs-t-a", State: "active",
		Destinations: []logs.Destination{cw("log_OK", "ok", true), bad, withToken, envRef}}}
	c, st := &cluster{}, &statuses{}
	out, err := newCtl(t, s, c, st).Reconcile(t.Context(), engine.NewRef(logs.Kind, teamID, teamID))
	if !errors.Is(err, engine.ErrTerminal) {
		t.Fatalf("err = %v", err)
	}
	if len(c.applied) != 1 || len(c.applied[0].Outputs) != 1 || c.applied[0].Outputs[0].ID != "log_OK" {
		t.Fatalf("outputs %+v", c.applied)
	}
	if cd, _ := cond(out, logs.CondOutputsReady); cd.Status != domain.ConditionFalse || !strings.Contains(cd.Message, "log_BAD") {
		t.Fatalf("%+v", out.Conditions)
	}
	bySt := map[string]engine.Report{}
	for _, r := range st.reports {
		bySt[r.ID] = r
	}
	if r := bySt["log_BAD"]; r.Phase != domain.PhaseFailed || r.Conditions[0].Status != domain.ConditionFalse {
		t.Fatalf("bad destination status %+v", r)
	}
	if r := bySt["log_OK"]; r.Phase != domain.PhaseReady {
		t.Fatalf("ok destination status %+v", r)
	}
}

func TestTeamStatesAndFailures(t *testing.T) {
	ref := engine.NewRef(logs.Kind, teamID, teamID)
	for _, state := range []string{"deleting", "deleted"} {
		c := &cluster{}
		out, err := newCtl(t, &src{d: logs.Desired{TeamID: teamID, State: state, Destinations: []logs.Destination{cw("log_A", "cw", true)}}}, c, nil).Reconcile(t.Context(), ref)
		if err != nil || !out.NoStatus || len(c.applied)+len(c.removed) != 0 {
			t.Fatalf("%s: %+v %v", state, out, err)
		}
	}
	// A suspended team keeps its log delivery.
	c := &cluster{}
	if _, err := newCtl(t, &src{d: logs.Desired{TeamID: teamID, Namespace: "afs-t-a", State: "suspended", Destinations: []logs.Destination{cw("log_A", "cw", true)}}}, c, nil).Reconcile(t.Context(), ref); err != nil || len(c.applied) != 1 {
		t.Fatalf("suspended: %v %d", err, len(c.applied))
	}
	// A vanished team is not an error and writes no status.
	if out, err := newCtl(t, &src{err: domain.NotFound("team")}, &cluster{}, nil).Reconcile(t.Context(), ref); err != nil || !out.NoStatus {
		t.Fatalf("not found: %+v %v", out, err)
	}
	if _, err := newCtl(t, &src{err: errors.New("db down")}, &cluster{}, nil).Reconcile(t.Context(), ref); err == nil {
		t.Fatal("a load failure was swallowed")
	}
	// A cluster failure is attributed to AggregatorReady and retried.
	boom := errors.New("apiserver down")
	cl := &cluster{applyErr: boom}
	s := &src{d: logs.Desired{TeamID: teamID, Namespace: "afs-t-a", State: "active", Destinations: []logs.Destination{cw("log_A", "cw", true)}}}
	_, err := newCtl(t, s, cl, nil).Reconcile(t.Context(), ref)
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != logs.CondAggregatorReady || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	cl = &cluster{removeErr: boom}
	s.d.Destinations = nil
	if _, err := newCtl(t, s, cl, nil).Reconcile(t.Context(), ref); !errors.As(err, &ce) {
		t.Fatalf("remove failure: %v", err)
	}
}

func TestConfigChecks(t *testing.T) {
	if _, err := logs.NewController(logs.Config{Cluster: &cluster{}, MasterKey: master}); err == nil {
		t.Error("missing source accepted")
	}
	if _, err := logs.NewController(logs.Config{Source: &src{}, Cluster: &cluster{}, MasterKey: []byte("short")}); err == nil {
		t.Error("short master key accepted")
	}
	r := logs.Route()
	if r.Table != "log_destinations" || len(r.Targets) != 1 || r.Targets[0].Kind != logs.Kind || r.Targets[0].Self {
		t.Fatalf("route %+v", r)
	}
	if c := (&logs.Controller{}); c.Kind() != logs.Kind {
		t.Fatal("kind")
	}
}
