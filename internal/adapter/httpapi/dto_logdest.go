package httpapi

import (
	"encoding/json"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// logDestinationBody is the wire shape of LogDestination (API contract §5, camelCase).
type logDestinationBody struct {
	ID         string            `json:"id"`
	Version    int64             `json:"version"`
	CreatedAt  string            `json:"createdAt"`
	CreatedBy  string            `json:"createdBy"`
	UpdatedAt  string            `json:"updatedAt"`
	Generation int64             `json:"generation"`
	Status     statusBody        `json:"status"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Config     map[string]string `json:"config"`
	SecretID   *string           `json:"secretId"`
	Enabled    bool              `json:"enabled"`
}

// statusBody is the wire shape of Status.
type statusBody struct {
	Phase              string             `json:"phase"`
	ObservedGeneration int64              `json:"observedGeneration"`
	Conditions         []domain.Condition `json:"conditions"`
	UpdatedAt          string             `json:"updatedAt"`
}

func toStatusBody(s domain.Status) statusBody {
	c := s.Conditions
	if c == nil {
		c = []domain.Condition{}
	}
	return statusBody{Phase: string(s.Phase), ObservedGeneration: s.ObservedGeneration, Conditions: c, UpdatedAt: w2Time(s.UpdatedAt)}
}

func toLogDestinationBody(d domain.LogDestination) logDestinationBody {
	var sec *string
	if d.SecretID != "" {
		s := d.SecretID
		sec = &s
	}
	cfg := d.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	return logDestinationBody{ID: d.ID, Version: int64(d.Version), CreatedAt: w2Time(d.CreatedAt), CreatedBy: d.CreatedBy,
		UpdatedAt: w2Time(d.UpdatedAt), Generation: int64(d.Generation), Status: toStatusBody(d.Status), Name: d.Name,
		Kind: string(d.Kind), Config: cfg, SecretID: sec, Enabled: d.Enabled}
}

// createLogDestinationRequest is the body of CreateLogDestination.
type createLogDestinationRequest struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Config   map[string]string `json:"config"`
	SecretID string            `json:"secretId"`
	Enabled  *bool             `json:"enabled"`
}

// updateLogDestinationRequest is the body of UpdateLogDestination; absent members are unchanged.
type updateLogDestinationRequest struct {
	Name     *string            `json:"name"`
	Config   map[string]string  `json:"config"`
	SecretID w2Nullable[string] `json:"secretId"`
	Enabled  *bool              `json:"enabled"`
}

// logDestinationMembers are the members a log destination request may carry.
var logDestinationMembers = map[string]bool{"name": true, "kind": true, "config": true, "secretId": true, "enabled": true}

// refuseInlineCredential turns an unknown body member that looks like a credential into a
// ValidationError that says what to do instead (RA-US-17.1): a destination never holds a value,
// only the id of the secret that does.
func refuseInlineCredential(raw []byte) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil // Decode reports malformed JSON
	}
	for k := range top {
		if !logDestinationMembers[k] && domain.InlineCredentialKey(k) {
			return bad(k, "credentials are never accepted inline; store the value as a secret and pass its id as secretId")
		}
	}
	return nil
}
