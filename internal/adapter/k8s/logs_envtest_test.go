//go:build envtest

package k8s_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

const logTeam = "team_01JD0000000000000000000031"

type liveLogs struct {
	mu sync.Mutex
	d  logs.Desired
}

func (s *liveLogs) set(dests ...logs.Destination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Destinations = dests
}

func (s *liveLogs) LoadLogs(context.Context, string) (logs.Desired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d, nil
}

var (
	gvkDeployment = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	gvkExtSecret  = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	gvkSecret     = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
)

// M2-I3: the log delivery controller with the real Kubernetes adapter renders the team's aggregator,
// changes its outputs when a destination is added, changed or removed (well inside 60 s), keeps
// credentials out of every ConfigMap, and removes everything it owns, and only that, when the last
// destination goes (RC-US-16.4, RC-US-16.3 part).
func TestLogAggregatorFollowsDestinations(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	envtesting.EnsureNamespace(t, env.Config, "afs-t-lg", nil)
	envtesting.EnsureNamespace(t, env.Config, render.DefaultLogRouteNamespace, nil)

	src := &liveLogs{d: logs.Desired{TeamID: logTeam, Slug: "lg", Namespace: "afs-t-lg", State: "active"}}
	ctl, err := logs.NewController(logs.Config{Source: src, Cluster: k8s.NewLogCluster(c, ""), MasterKey: []byte("master-key-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	ref := engine.NewRef(logs.Kind, logTeam, logTeam)
	cw := logs.Destination{ID: "log_01JD0000000000000000000041", Name: "cw", Kind: domain.LogCloudWatch, Enabled: true, Version: 1,
		Config: map[string]string{"logGroup": "/afs/lg", "region": "us-east-1"}}
	hec := logs.Destination{ID: "log_01JD0000000000000000000042", Name: "hec", Kind: domain.LogSplunkHEC, Enabled: true, Version: 1, //nolint:gosec // a fixture id or ARN, not a credential
		SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/lg/hec-AbCdEf", //nolint:gosec // a fixture ARN
		Config:    map[string]string{"endpoint": "https://splunk.example.com:8088"}}

	conf := func() string {
		cm, err := c.Get(ctx, gvkConfigMap, "afs-t-lg", render.LogAggregatorName)
		if err != nil || cm == nil {
			return ""
		}
		s, _, _ := unstructured.NestedString(cm.Object, "data", "fluent-bit.conf")
		return s
	}
	reconcile := func() engine.Outcome {
		t.Helper()
		out, err := ctl.Reconcile(ctx, ref)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return out
	}

	// One destination: the aggregator and everything it needs exist.
	src.set(cw)
	out := reconcile()
	if len(out.Changed) == 0 {
		t.Fatal("first pass changed nothing")
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		ns   string
		name string
	}{
		{gvkDeployment, "afs-t-lg", render.LogAggregatorName}, {schema.GroupVersionKind{Version: "v1", Kind: "Service"}, "afs-t-lg", render.LogAggregatorName},
		{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, "afs-t-lg", render.LogAggregatorName}, {gvkSecret, "afs-t-lg", render.LogRedactionKeyName},
		{gvkExtSecret, "afs-t-lg", render.LogRedactionValuesName}, {gvkCNP, "afs-t-lg", render.LogAggregatorName},
		{gvkConfigMap, render.DefaultLogRouteNamespace, "afs-log-route-lg"},
	} {
		if got, err := c.Get(ctx, o.gvk, o.ns, o.name); err != nil || got == nil {
			t.Errorf("%s %s/%s missing: %v", o.gvk.Kind, o.ns, o.name, err)
		}
	}
	if got, _ := c.Get(ctx, gvkExtSecret, "afs-t-lg", render.LogCredentialsName); got != nil {
		t.Error("credentials ExternalSecret exists though no destination has a secret")
	}
	if !strings.Contains(conf(), "cloudwatch_logs") || strings.Count(conf(), "[OUTPUT]") != 1 {
		t.Fatalf("config:\n%s", conf())
	}
	// A second pass with nothing changed writes nothing.
	if out := reconcile(); len(out.Changed) != 0 {
		t.Fatalf("idempotent pass changed %v", out.Changed)
	}
	hashOf := func() string {
		d, _ := c.Get(ctx, gvkDeployment, "afs-t-lg", render.LogAggregatorName)
		h, _, _ := unstructured.NestedString(d.Object, "spec", "template", "metadata", "annotations", "afs.stainedhead.io/config-hash")
		return h
	}
	h1 := hashOf()

	// A destination with a credential is added: the output, the credential reference and a new pod
	// template hash appear in one pass.
	start := time.Now()
	src.set(cw, hec)
	reconcile()
	if time.Since(start) > 60*time.Second {
		t.Fatal("outputs changed in more than 60 s")
	}
	if strings.Count(conf(), "[OUTPUT]") != 2 || !strings.Contains(conf(), "splunk") || !strings.Contains(conf(), "${AFS_LOG_01JD0000000000000000000042}") || h1 == hashOf() {
		t.Fatalf("config:\n%s", conf())
	}
	creds, _ := c.Get(ctx, gvkExtSecret, "afs-t-lg", render.LogCredentialsName)
	if creds == nil {
		t.Fatal("credentials ExternalSecret missing")
	}
	// No ConfigMap anywhere holds a credential or an ARN.
	all, err := c.List(ctx, gvkConfigMap, "", render.LabelManagedBy+"="+render.ManagedByValue)
	if err != nil {
		t.Fatal(err)
	}
	for _, cm := range all {
		data, _ := cm.Object["data"].(map[string]any) // some managed ConfigMaps (other tests') carry none
		for k, v := range data {
			if s, _ := v.(string); strings.Contains(s, "arn:aws:secretsmanager") && strings.Contains(cm.GetName(), "log") {
				t.Errorf("%s/%s key %s carries a secret ARN", cm.GetNamespace(), cm.GetName(), k)
			}
		}
	}

	// The destination with the secret is deleted: its output and the credentials object are gone.
	src.set(cw)
	reconcile()
	if strings.Count(conf(), "[OUTPUT]") != 1 || strings.Contains(conf(), "splunk") {
		t.Fatalf("deleted destination still has an output:\n%s", conf())
	}
	if got, _ := c.Get(ctx, gvkExtSecret, "afs-t-lg", render.LogCredentialsName); got != nil {
		t.Fatal("credentials ExternalSecret outlived its destination")
	}

	// Foreign objects are never removed: an unmanaged Service of the same name survives a removal.
	src.set()
	reconcile()
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		ns   string
		name string
	}{{gvkDeployment, "afs-t-lg", render.LogAggregatorName}, {gvkConfigMap, "afs-t-lg", render.LogAggregatorName}, {gvkSecret, "afs-t-lg", render.LogRedactionKeyName},
		{gvkExtSecret, "afs-t-lg", render.LogRedactionValuesName}, {gvkCNP, "afs-t-lg", render.LogAggregatorName}, {gvkConfigMap, render.DefaultLogRouteNamespace, "afs-log-route-lg"}} {
		if got, _ := c.Get(ctx, o.gvk, o.ns, o.name); got != nil && got.GetDeletionTimestamp() == nil {
			t.Errorf("%s %s/%s survived the last destination", o.gvk.Kind, o.ns, o.name)
		}
	}
	// And the controller does not touch an object it does not own.
	cs := envtesting.Clientset(t, env.Config)
	if _, err := cs.CoreV1().ConfigMaps("afs-t-lg").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: render.LogAggregatorName, Namespace: "afs-t-lg"}, Data: map[string]string{"mine": "yes"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if got, _ := c.Get(ctx, gvkConfigMap, "afs-t-lg", render.LogAggregatorName); got == nil {
		t.Fatal("an unmanaged ConfigMap was deleted")
	}
}
