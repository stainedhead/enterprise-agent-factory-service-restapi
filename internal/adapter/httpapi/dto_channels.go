package httpapi

import (
	"encoding/json"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// deliveryStatusBody is the wire shape of DeliveryStatus.
type deliveryStatusBody struct {
	Healthy         *bool   `json:"healthy"`
	LastDeliveredAt *string `json:"lastDeliveredAt"`
	LastError       *string `json:"lastError"`
	LastFailureAt   *string `json:"lastFailureAt"`
	LastTestID      *string `json:"lastTestId"`
}

// channelBody is the wire shape of NotificationChannel (API contract §5, camelCase).
type channelBody struct {
	ID         string             `json:"id"`
	Version    int64              `json:"version"`
	CreatedAt  string             `json:"createdAt"`
	CreatedBy  string             `json:"createdBy"`
	UpdatedAt  string             `json:"updatedAt"`
	Generation int64              `json:"generation"`
	Status     statusBody         `json:"status"`
	Delivery   deliveryStatusBody `json:"delivery"`
	Name       string             `json:"name"`
	Kind       string             `json:"kind"`
	Config     map[string]any     `json:"config"`
	SecretID   *string            `json:"secretId"`
	EventTypes []string           `json:"eventTypes"`
	Enabled    bool               `json:"enabled"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toChannelBody(c domain.NotificationChannel) channelBody {
	cfg := c.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	ev := c.EventTypes
	if ev == nil {
		ev = []string{}
	}
	d := c.Delivery()
	return channelBody{ID: c.ID, Version: int64(c.Version), CreatedAt: w2Time(c.CreatedAt), CreatedBy: c.CreatedBy,
		UpdatedAt: w2Time(c.UpdatedAt), Generation: int64(c.Generation), Status: toStatusBody(c.Status),
		Delivery: deliveryStatusBody{Healthy: d.Healthy, LastDeliveredAt: w2TimePtr(d.LastDeliveredAt), LastError: optStr(d.LastError),
			LastFailureAt: w2TimePtr(d.LastFailureAt), LastTestID: optStr(d.LastTestID)},
		Name: c.Name, Kind: string(c.Kind), Config: cfg, SecretID: optStr(c.SecretID), EventTypes: ev, Enabled: c.Enabled}
}

// testResultBody is the wire shape of NotificationTestResult.
type testResultBody struct {
	TestID    string  `json:"testId"`
	Delivered *bool   `json:"delivered,omitempty"`
	Error     *string `json:"error,omitempty"`
	Pending   *bool   `json:"pending,omitempty"`
}

func toTestResultBody(r domain.TestResult) testResultBody {
	if r.Pending {
		t := true
		return testResultBody{TestID: r.TestID, Pending: &t}
	}
	f := false
	return testResultBody{TestID: r.TestID, Delivered: &r.Delivered, Error: optStr(r.Error), Pending: &f}
}

// createChannelRequest is the body of the two create operations.
type createChannelRequest struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Config     map[string]any `json:"config"`
	SecretID   string         `json:"secretId"`
	EventTypes []string       `json:"eventTypes"`
	Enabled    *bool          `json:"enabled"`
}

func (r createChannelRequest) params() domain.NewChannelParams {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	return domain.NewChannelParams{Name: r.Name, Kind: domain.ChannelKind(r.Kind), Config: r.Config, SecretID: r.SecretID,
		EventTypes: r.EventTypes, Enabled: enabled}
}

// updateChannelRequest is the body of the two update operations; absent members are unchanged.
type updateChannelRequest struct {
	Name       *string            `json:"name"`
	Config     map[string]any     `json:"config"`
	SecretID   w2Nullable[string] `json:"secretId"`
	EventTypes *[]string          `json:"eventTypes"`
	Enabled    *bool              `json:"enabled"`
}

func (r updateChannelRequest) patch() domain.ChannelPatch {
	p := domain.ChannelPatch{Name: r.Name, Config: r.Config, EventTypes: r.EventTypes, Enabled: r.Enabled}
	if r.SecretID.Set {
		if r.SecretID.Null {
			p.ClearSecret = true
		} else {
			p.SecretID = &r.SecretID.Value
		}
	}
	return p
}

// channelMembers are the members a channel request may carry.
var channelMembers = map[string]bool{"name": true, "kind": true, "config": true, "secretId": true, "eventTypes": true, "enabled": true}

// refuseInlineChannelCredential turns an unknown body member that looks like a credential into a
// ValidationError that says what to do instead: a channel never holds a value, only the id of the
// secret that does.
func refuseInlineChannelCredential(raw []byte) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil // Decode reports malformed JSON
	}
	for k := range top {
		if !channelMembers[k] && domain.InlineCredentialKey(k) {
			return bad(k, "credentials are never accepted inline; store the value as a secret and pass its id as secretId")
		}
	}
	return nil
}
