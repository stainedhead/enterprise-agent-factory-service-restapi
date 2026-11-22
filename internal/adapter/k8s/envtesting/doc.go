// Package envtesting starts a real kube-apiserver and etcd (controller-runtime envtest) with the pinned
// custom resource definitions of testdata/crds, for the suites that must prove behavior against the
// Kubernetes API: server-side apply, drift, leader election and admission. The harness itself is behind
// the envtest build tag (make envtest); KUBEBUILDER_ASSETS must name a directory holding kube-apiserver
// and etcd. Suites skip when it is not set.
package envtesting
