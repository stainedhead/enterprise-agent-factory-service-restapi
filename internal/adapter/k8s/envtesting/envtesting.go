//go:build envtest

package envtesting

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Env is a running control plane.
type Env struct {
	env *envtest.Environment
	// Config connects as cluster admin.
	Config *rest.Config
}

// CRDDir is the directory of the pinned custom resource definitions.
func CRDDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", "crds")
}

// Run starts the control plane, runs the package's tests with it and stops it. Use it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(envtesting.Run(m, &env)) }
//
// When KUBEBUILDER_ASSETS is unset the tests still run, with a nil *Env, and are expected to call
// Require, which skips them.
func Run(m *testing.M, out **Env) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run()
	}
	e := &envtest.Environment{CRDDirectoryPaths: []string{CRDDir()}, ErrorIfCRDPathMissing: true}
	cfg, err := e.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "envtesting: start:", err)
		return 1
	}
	*out = &Env{env: e, Config: cfg}
	code := m.Run()
	if err := e.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "envtesting: stop:", err)
	}
	return code
}

// Require skips t when no control plane is running.
func Require(t testing.TB, e *Env) *Env {
	t.Helper()
	if e == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make envtest` with kube-apiserver and etcd available")
	}
	return e
}

// Clientset returns a typed client for cfg.
func Clientset(t testing.TB, cfg *rest.Config) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// As returns a copy of cfg that acts as user (admin impersonation), for admission tests.
func As(cfg *rest.Config, user string, groups ...string) *rest.Config {
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	return c
}

// EnsureNamespace creates a namespace if it does not exist, with labels.
func EnsureNamespace(t testing.TB, cfg *rest.Config, name string, labels map[string]string) {
	t.Helper()
	_, err := Clientset(t, cfg).CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// Eventually polls cond every 50 ms until it is true or d elapses.
func Eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}
