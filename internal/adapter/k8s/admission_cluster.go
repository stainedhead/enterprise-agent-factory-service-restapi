package k8s

import (
	"context"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
)

// AdmissionCluster implements admission.Cluster with server-side apply.
type AdmissionCluster struct{ client *Client }

// NewAdmissionCluster returns an AdmissionCluster.
func NewAdmissionCluster(c *Client) *AdmissionCluster { return &AdmissionCluster{client: c} }

var _ admission.Cluster = (*AdmissionCluster)(nil)

// ApplyAdmission implements admission.Cluster. Every object is attempted; the first error is returned.
func (a *AdmissionCluster) ApplyAdmission(ctx context.Context, s admission.Spec) (admission.ApplyResult, error) {
	var res admission.ApplyResult
	var firstErr error
	for _, o := range render.AdmissionObjects(s) {
		r, err := a.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return res, firstErr
}
