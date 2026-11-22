//go:build envtest

package k8s_test

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

type idleFeed struct{}

func (idleFeed) Listen(ctx context.Context, h engine.FeedHandler) error {
	if h.Connected != nil {
		h.Connected()
	}
	<-ctx.Done()
	return nil
}

type listRefs struct{ refs []engine.Ref }

func (l listRefs) List(context.Context, []engine.Kind) ([]engine.Ref, error)  { return l.refs, nil }
func (l listRefs) Stale(context.Context, []engine.Kind) ([]engine.Ref, error) { return nil, nil }

type memStatus struct {
	mu   sync.Mutex
	last engine.Report
	n    int
	// sawDrift is set when any report carried DriftCorrected (the real writer keeps it for 10 min).
	sawDrift bool
}

func (s *memStatus) Apply(_ context.Context, r engine.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = r
	for _, c := range r.Conditions {
		s.sawDrift = s.sawDrift || c.Type == domain.CondDriftCorrected
	}
	s.n++
	return nil
}

func (s *memStatus) snapshot() engine.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

type memDrift struct {
	mu   sync.Mutex
	recs []engine.DriftRecord
}

func (d *memDrift) Record(_ context.Context, r engine.DriftRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recs = append(d.recs, r)
	return nil
}

func (d *memDrift) objects() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, r := range d.recs {
		out = append(out, r.Object)
	}
	return out
}

// M1-F6: an edit and a delete of owned objects are reverted within 60 s, each recorded once, and the
// status carries DriftCorrected.
func TestDriftIsRevertedAndRecordedOnce(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const id = "team_01JD0000000000000000000012"
	d := team.Desired{ID: id, Slug: "f6", Namespace: "afs-t-f6", DisplayName: "F6", Kind: "persistent", State: "active", Generation: 2,
		Quotas: team.Quotas{CPU: "4", Memory: "8Gi", Pods: 10}}
	envtesting.EnsureNamespace(t, env.Config, "afs-system", nil)
	ca := render.New("v1", "ConfigMap", "afs-system", "afs-gateway-ca")
	ca.Object["data"] = map[string]any{render.GatewayCAKey: "pem"}
	if _, err := c.Apply(ctx, render.Managed(ca, render.OwnerPlatform())); err != nil {
		t.Fatal(err)
	}
	ctl, err := team.NewController(team.Config{
		Source: teamtest.NewSource(d), Cluster: k8s.NewTeamCluster(c, k8s.GatewayCASource{Namespace: "afs-system", Name: "afs-gateway-ca"}),
		IAM: teamtest.NewIAM(), Profiles: teamtest.NewProfiles(),
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", SecretsPrefix: "afs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := engine.NewRegistry(engine.Route{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}})
	ref := engine.NewRef(engine.KindTeam, id, id)
	st, rec := &memStatus{}, &memDrift{}
	e, err := engine.New(engine.Config{Feed: idleFeed{}, Lister: listRefs{refs: []engine.Ref{ref}}, Registry: reg,
		Controllers: []engine.Controller{ctl}, Status: st, Drift: rec, PollInterval: time.Hour, ResyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e.Run(ctx) }()
	defer func() { t.Logf("status n=%d last=%+v records=%v", st.n, st.snapshot(), rec.objects()) }()

	gvkNP := schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
	gvkQuota := schema.GroupVersionKind{Version: "v1", Kind: "ResourceQuota"}
	envtesting.Eventually(t, 60*time.Second, "initial convergence", func() bool {
		o, _ := c.Get(ctx, gvkNP, "afs-t-f6", render.DenyIngressName)
		return o != nil && st.snapshot().Phase == domain.PhaseReady
	})
	w, err := k8s.NewWatcher(c, func(r engine.Ref, object string) { e.NoteDrift(r, object) }, nil, gvkNP, gvkQuota)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(ctx) }()
	time.Sleep(2 * time.Second) // informers synced

	// Delete behind the client's back, as another actor would (Client.Delete records an own write).
	if err := c.Dynamic().Resource(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}).
		Namespace("afs-t-f6").Delete(ctx, render.DenyIngressName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"spec":{"hard":{"pods":"999"}}}`)
	if _, err := c.Dynamic().Resource(schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}).Namespace("afs-t-f6").
		Patch(ctx, render.QuotaName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	envtesting.Eventually(t, 60*time.Second, "revert", func() bool {
		o, _ := c.Get(ctx, gvkNP, "afs-t-f6", render.DenyIngressName)
		q, _ := c.Get(ctx, gvkQuota, "afs-t-f6", render.QuotaName)
		if o == nil || q == nil {
			return false
		}
		hard, _, _ := nestedString(q.Object, "spec", "hard", "pods")
		return hard == "10"
	})
	envtesting.Eventually(t, 30*time.Second, "audit records", func() bool { return len(rec.objects()) >= 2 })
	time.Sleep(3 * time.Second)
	objs := rec.objects()
	seen := map[string]int{}
	for _, o := range objs {
		seen[o]++
	}
	for o, n := range seen {
		if n != 1 {
			t.Errorf("drift of %s recorded %d times, want once", o, n)
		}
	}
	st.mu.Lock()
	saw := st.sawDrift
	st.mu.Unlock()
	if !saw {
		t.Error("no status report carried DriftCorrected")
	}
}

func nestedString(obj map[string]any, fields ...string) (string, bool, error) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m[f]
	}
	s, ok := cur.(string)
	return s, ok, nil
}
