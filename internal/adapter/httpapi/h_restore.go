package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

// The restore handlers are their own area: the revisions area is bound by the M1 revision reads.
func init() { Areas = append(Areas, Area{"restore", none}) }

// RestoreAPI serves RestoreRevision (M4-G, RA-13).
type RestoreAPI struct {
	Svc *restore.Service
}

// BindRestore binds the restore handler as the restore area.
func BindRestore(a *RestoreAPI) { w2SetArea("restore", a.Register) }

// Register binds the restore operation.
func (a *RestoreAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"RestoreRevision": a.restore})
}

type restoreChangeBody struct {
	ResourceKind string             `json:"resourceKind"`
	ResourceID   string             `json:"resourceId"`
	Action       string             `json:"action"`
	Fields       []restoreFieldBody `json:"fields,omitempty"`
	Reason       *string            `json:"reason"`
}

type restoreFieldBody struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

type restoreBody struct {
	Revision int64               `json:"revision"`
	DryRun   bool                `json:"dryRun"`
	Changes  []restoreChangeBody `json:"changes"`
	Skipped  []restoreChangeBody `json:"skipped"`
}

func toRestoreChanges(in []domain.RestoreChange) []restoreChangeBody {
	out := make([]restoreChangeBody, 0, len(in))
	for _, c := range in {
		b := restoreChangeBody{ResourceKind: c.ResourceKind, ResourceID: c.ResourceID, Action: string(c.Action)}
		for _, f := range c.Fields {
			b.Fields = append(b.Fields, restoreFieldBody{Field: f.Field, From: f.From, To: f.To})
		}
		if c.Reason != "" {
			reason := c.Reason
			b.Reason = &reason
		}
		out = append(out, b)
	}
	return out
}

func toRestoreBody(r restore.Result) restoreBody {
	return restoreBody{Revision: r.Revision, DryRun: r.DryRun, Changes: toRestoreChanges(r.Changes), Skipped: toRestoreChanges(r.Skipped)}
}

func (a *RestoreAPI) restore(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	rev, err := w2Int64("rev", r.PathValue("rev"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		DryRun bool `json:"dryRun"`
	}
	if len(raw) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	idem, err := w2Idem(r, "RestoreRevision", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toRestoreBody(v.(restore.Result)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Run(r.Context(), c, teamID, rev, req.DryRun, expect, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusOK, toRestoreBody(res))
}
