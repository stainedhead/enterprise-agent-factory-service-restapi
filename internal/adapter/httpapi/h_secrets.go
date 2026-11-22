package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// SecretsAPI serves the secret operations (M2-C).
type SecretsAPI struct {
	Svc   *secrets.Service
	Pages PageTokens
	// IdemKey is the server key (at least 32 bytes, the same on every replica) of the idempotency
	// fingerprint of secret values (FR-R14).
	IdemKey []byte
}

// BindSecrets binds the secret handlers as the secrets area.
func BindSecrets(a *SecretsAPI) Registrar { return a.Register }

// Register binds the secret operations.
func (a *SecretsAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"ListSecrets": a.list, "CreateSecret": a.create, "GetSecret": a.get, "UpdateSecret": a.update,
		"DeleteSecret": a.del, "PutSecretValue": a.putValue, "RotateSecret": a.rotate,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *SecretsAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), domain.SecretManagedBy(r.URL.Query().Get("managedBy")), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[secretBody]{Items: make([]secretBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, s := range rows {
		out.Items = append(out.Items, toSecretBody(s))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *SecretsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), r.PathValue("secretId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toSecretBody(s))
}

func (a *SecretsAPI) create(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createSecretRequest
	if err := decodeSecretBody(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	val, err := secretValue(req.Value)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rot, err := req.Rotation.domain("rotation")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	fp, err := idemBody(a.IdemKey, raw, req.Value)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateSecret", teamID, fp, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toSecretBody(v.(domain.Secret)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, domain.NewSecretParams{Scope: domain.SecretScope(req.Scope), AgentID: req.AgentID,
		PoolID: req.PoolID, Name: req.Name, Description: req.Description, ExternalSystemID: req.ExternalSystemID, Rotation: rot, Value: val}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Secret.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/secrets/"+res.Secret.ID)
	WriteJSON(w, http.StatusCreated, toSecretBody(res.Secret))
}

func (a *SecretsAPI) update(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateSecretRequest
	if err := decodeSecretBody(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := domain.SecretPatch{Description: req.Description}
	if req.ExternalSystemID.Set {
		p.ExternalSystemSet, p.ExternalSystemNull, p.ExternalSystemID = true, req.ExternalSystemID.Null, req.ExternalSystemID.Value
	}
	if req.Rotation != nil {
		rot, err := req.Rotation.domain("rotation")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p.Rotation = &rot
	}
	s, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("secretId"), expect, p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toSecretBody(s))
}

func (a *SecretsAPI) del(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("secretId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SecretsAPI) putValue(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req valueRequest
	if err := decodeSecretBody(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	val, err := secretValue(req.Value)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Svc.PutValue(r.Context(), c, r.PathValue("teamId"), r.PathValue("secretId"), expect, val); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SecretsAPI) rotate(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The body is optional: an AFS-managed secret is rotated with none (AFS generates the value).
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	var req valueRequest
	if err := decodeSecretBody(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var val domain.SecretValue
	if req.Value != nil {
		if val, err = secretValue(req.Value); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	fp, err := idemBody(a.IdemKey, raw, req.Value)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "RotateSecret", teamID, fp, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toSecretBody(v.(domain.Secret)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Rotate(r.Context(), c, teamID, r.PathValue("secretId"), val, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Secret.Version)
	WriteJSON(w, http.StatusOK, toSecretBody(res.Secret))
}
