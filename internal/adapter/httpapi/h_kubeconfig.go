package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/kubeconfig"
)

const kubeconfigAreaName = "kubeconfig"

func init() { Areas = append(Areas, Area{kubeconfigAreaName, none}) }

// KubeconfigAPI serves GetTeamKubeconfig (M5-E1, RA-10).
type KubeconfigAPI struct {
	Svc *kubeconfig.Service
}

// BindKubeconfig binds the kubeconfig handler as the kubeconfig area.
func BindKubeconfig(a *KubeconfigAPI) { w2SetArea(kubeconfigAreaName, a.Register) }

// Register binds the operation.
func (a *KubeconfigAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"GetTeamKubeconfig": a.get})
}

// get answers the kubeconfig as application/yaml. The file holds no credential but is still not stored
// by caches: it names the cluster and the team's namespace.
func (a *KubeconfigAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	doc, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc) //nolint:gosec // the document is rendered by this API, with no request data in it beyond the team's own slug
}
