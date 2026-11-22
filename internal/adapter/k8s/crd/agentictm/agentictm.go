// Package agentictm pins the Agentic Teammates custom resource the reconciler renders for every agent
// (OQ-7, root R6). Version, group and kind are constants here and nowhere else: when the operator's CRD is
// pinned for real, only this package changes. The resource is built from typed Kubernetes structs so the
// pod template is checked by the compiler, and handed to the apply as unstructured (ADR-006).
package agentictm

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// API group, version and kind of the pinned resource (OQ-7 default: the version current at M2).
const (
	Group   = "agenticteammates.stainedhead.io"
	Version = "v1alpha1"
	Kind    = "Teammate"
)

// GVK is the resource's group, version and kind.
func GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: Version, Kind: Kind}
}

// APIVersion is group/version.
func APIVersion() string { return Group + "/" + Version }

// Annotations of the pod template the reconciler sets.
const (
	// AnnotationPodTemplateHash is the hash of the referenced versions (agent.PodTemplateHash).
	AnnotationPodTemplateHash = "afs.stainedhead.io/pod-template-hash"
	// AnnotationRestartNonce changes on every restart request.
	AnnotationRestartNonce = "afs.stainedhead.io/restart-nonce"
)

// Spec is the spec of a Teammate: the StatefulSet the operator renders, with one replica (or none when
// paused), the pod template and the volume claim template whose claims are retained when the agent is
// scaled down or deleted (RC-16).
type Spec struct {
	// Template is the runtime template (harness) name; ModelID the AFS model id; Persona the agent's
	// persona text. The operator reads them; none is a secret.
	Template string `json:"template,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Persona  string `json:"persona,omitempty"`
	// Replicas is 1, or 0 when the agent is paused or its team is suspended.
	Replicas int32 `json:"replicas"`
	// ServiceName is the StatefulSet's governing service name.
	ServiceName string `json:"serviceName"`
	// PodTemplate is the pod of the agent.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
	// VolumeClaimTemplates holds the agent's single volume.
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates"`
	// PersistentVolumeClaimRetentionPolicy is Retain for scale-down and delete.
	PersistentVolumeClaimRetentionPolicy appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy `json:"persistentVolumeClaimRetentionPolicy"`
}

// Object returns the Teammate named name in namespace carrying spec.
func Object(namespace, name string, spec Spec) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return nil, fmt.Errorf("agentictm: convert spec: %w", err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(APIVersion())
	u.SetKind(Kind)
	u.SetName(name)
	u.SetNamespace(namespace)
	u.Object["spec"] = PruneNulls(m)
	return u, nil
}

// PruneNulls removes the null values and empty status stubs typed structs serialize ("creationTimestamp":
// null, "status": {}), so a server-side apply carries only what the reconciler means to own.
func PruneNulls(v map[string]any) map[string]any {
	out := map[string]any{}
	for k, val := range v {
		switch t := val.(type) {
		case nil:
			continue
		case map[string]any:
			p := PruneNulls(t)
			if len(p) == 0 && (k == "status" || k == "creationTimestamp") {
				continue
			}
			out[k] = p
		case []any:
			out[k] = pruneList(t)
		default:
			out[k] = val
		}
	}
	return out
}

func pruneList(l []any) []any {
	out := make([]any, 0, len(l))
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, PruneNulls(m))
			continue
		}
		out = append(out, e)
	}
	return out
}

// SpecOf reads the typed spec back from a live Teammate, for tests and drift checks.
func SpecOf(u *unstructured.Unstructured) (Spec, error) {
	raw, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("agentictm: read spec: %w", err)
	}
	return s, nil
}
