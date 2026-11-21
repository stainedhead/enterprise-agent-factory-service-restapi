package k8s

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
)

func namespaceCluster(t *testing.T, objs ...runtime.Object) *TeamCluster {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return NewTeamCluster(NewClientFor(dynfake.NewSimpleDynamicClient(scheme, objs...), resettable{m}), GatewayCASource{})
}

func ns(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// M1-I1: the namespace goes last; deleting asks for deletion and reports it gone only once it is.
func TestDeleteNamespaceRequestsThenReportsGone(t *testing.T) {
	c := namespaceCluster(t, ns("afs-t-pay", map[string]string{render.LabelManagedBy: render.ManagedByValue}))
	gone, err := c.DeleteNamespace(context.Background(), "afs-t-pay")
	if err != nil || gone {
		t.Fatalf("first call: gone %v err %v", gone, err)
	}
	gone, err = c.DeleteNamespace(context.Background(), "afs-t-pay")
	if err != nil || !gone {
		t.Fatalf("second call (namespace removed): gone %v err %v", gone, err)
	}
}

// The reconciler never deletes a namespace it did not create, nor one outside afs-t-*.
func TestDeleteNamespaceRefusesForeignNamespaces(t *testing.T) {
	c := namespaceCluster(t, ns("afs-t-foreign", nil), ns("kube-system", map[string]string{render.LabelManagedBy: render.ManagedByValue}))
	if _, err := c.DeleteNamespace(context.Background(), "afs-t-foreign"); !errors.Is(err, ErrNotManaged) {
		t.Errorf("unmanaged: %v", err)
	}
	if _, err := c.DeleteNamespace(context.Background(), "kube-system"); !errors.Is(err, ErrNotManaged) {
		t.Errorf("outside afs-t-*: %v", err)
	}
	if gone, err := c.DeleteNamespace(context.Background(), "afs-t-missing"); err != nil || !gone {
		t.Errorf("missing namespace: gone %v err %v", gone, err)
	}
}
