package render

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Teammate renders the Agentic Teammates resource of an agent: replicas 1 (0 when paused or when the team
// is suspended), the pod template, the volume claim template and the claim retention policy Retain for
// scale-down and delete (RC-16, RC-US-14).
func Teammate(env agent.Environment) (*unstructured.Unstructured, error) {
	d := env.Agent
	u, err := agentictm.Object(d.Team.Namespace, agent.WorkloadName(d.Name), agentictm.Spec{
		Template: d.Template, ModelID: d.ModelID, Persona: d.Persona,
		Replicas:             env.Replicas,
		ServiceName:          agent.WorkloadName(d.Name),
		PodTemplate:          PodTemplate(env),
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{VolumeClaimTemplate(d)},
		PersistentVolumeClaimRetentionPolicy: appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenScaled: appsv1.RetainPersistentVolumeClaimRetentionPolicyType, WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
	})
	if err != nil {
		return nil, err
	}
	u.SetLabels(map[string]string{LabelAgentID: d.ID})
	return Managed(u, OwnerOfAgent(d)), nil
}
