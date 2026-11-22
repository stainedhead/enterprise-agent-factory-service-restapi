package admission_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

type fakeCluster struct {
	got admission.Spec
	res admission.ApplyResult
	err error
}

func (f *fakeCluster) ApplyAdmission(_ context.Context, s admission.Spec) (admission.ApplyResult, error) {
	f.got = s
	return f.res, f.err
}

func TestNewControllerValidates(t *testing.T) {
	for name, tc := range map[string]struct {
		cluster admission.Cluster
		spec    admission.Spec
	}{
		"no cluster":  {nil, admission.Spec{ReconcilerUsers: []string{"r"}}},
		"no reconcil": {&fakeCluster{}, admission.Spec{}},
	} {
		if _, err := admission.NewController(tc.cluster, tc.spec); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestReconcile(t *testing.T) {
	f := &fakeCluster{res: admission.ApplyResult{Changed: []string{"ValidatingAdmissionPolicy/x"}}}
	c, err := admission.NewController(f, admission.Spec{ReconcilerUsers: []string{"afs-reconciler"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind() != engine.KindAdmission {
		t.Fatal("kind")
	}
	out, err := c.Reconcile(t.Context(), admission.Ref)
	if err != nil || !out.NoStatus || len(out.Changed) != 1 || f.got.ReconcilerUsers[0] != "afs-reconciler" {
		t.Fatalf("out=%+v err=%v spec=%+v", out, err, f.got)
	}
	f.err = errors.New("boom")
	if _, err := c.Reconcile(t.Context(), admission.Ref); err == nil {
		t.Fatal("want error")
	}
}
