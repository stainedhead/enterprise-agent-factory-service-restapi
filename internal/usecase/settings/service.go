// Package settings implements GetSettings and PutSetting (API contract §4.11, root PRD §6.13).
// Values are typed and range-checked from the generated registry (domain.SettingSpecs); the database
// checks them again. Contract §7 has no settings event, so a change writes the row and an audit
// record only (CD-7).
package settings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Stored is a setting row.
type Stored struct {
	Value     json.RawMessage
	Version   domain.Version
	UpdatedAt time.Time
}

// Repository is the service_settings store.
type Repository interface {
	// All returns every stored setting by key.
	All(ctx context.Context, tx port.Tx) (map[string]Stored, error)
	// Put upserts key. With expect set the row must exist at that version (PreconditionFailed
	// otherwise).
	Put(ctx context.Context, tx port.Tx, key string, value json.RawMessage, expect *domain.Version) (Stored, error)
}

// Setting is one setting as GetSettings reports it.
type Setting struct {
	Spec      domain.SettingSpec
	Value     json.RawMessage
	Version   domain.Version
	UpdatedAt *time.Time
}

// Service is the settings use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string) error {
	if !authz.Authorize(c.Principal, authz.AdminSettings, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", authz.AdminSettings)
	}
	return nil
}

func build(spec domain.SettingSpec, st Stored, ok bool) Setting {
	if !ok {
		return Setting{Spec: spec, Value: json.RawMessage(spec.Default)}
	}
	u := st.UpdatedAt
	return Setting{Spec: spec, Value: st.Value, Version: st.Version, UpdatedAt: &u}
}

// Get implements GetSettings: every key of the registry with its value (the default when unset).
func (s *Service) Get(ctx context.Context, c acting.Caller) ([]Setting, error) {
	if err := s.check(ctx, c, "GetSettings"); err != nil {
		return nil, err
	}
	var stored map[string]Stored
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		stored, err = s.Repo.All(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]Setting, 0, len(domain.AllSettingSpecs()))
	for _, spec := range domain.AllSettingSpecs() {
		st, ok := stored[spec.Key]
		out = append(out, build(spec, st, ok))
	}
	return out, nil
}

// Put implements PutSetting: unknown key or a value outside the type or range is a ValidationError.
func (s *Service) Put(ctx context.Context, c acting.Caller, key string, value json.RawMessage, expect *domain.Version) (Setting, error) {
	if err := s.check(ctx, c, "PutSetting"); err != nil {
		return Setting{}, err
	}
	if next, renamed := domain.RenamedSettings[key]; renamed {
		// The 1.0.1 keys are read-only aliases until 2.0.0 (ORC-27): the error names the new key.
		e := domain.Validation(domain.FieldError{Field: "key", Reason: "setting_renamed: use " + next})
		e.Reason = "setting_renamed"
		e.Details = map[string]any{"reason": "setting_renamed", "key": key, "newKey": next}
		return Setting{}, e
	}
	spec, ok := domain.SettingSpecFor(key)
	if !ok {
		return Setting{}, domain.Validation(domain.FieldError{Field: "key", Reason: "unknown setting " + key})
	}
	if err := spec.Validate(value); err != nil {
		return Setting{}, err
	}
	if err := domain.ValidateCollabSetting(key, value); err != nil {
		return Setting{}, err
	}
	var out Setting
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		all, err := s.Repo.All(ctx, tx)
		if err != nil {
			return err
		}
		before, had := all[key]
		st, err := s.Repo.Put(ctx, tx, key, value, expect)
		if err != nil {
			return err
		}
		var prev any
		if had {
			prev = before.Value
		} else {
			prev = json.RawMessage(spec.Default)
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutSetting", "service_settings", key, "",
			map[string]any{"key": key, "value": prev}, map[string]any{"key": key, "value": st.Value})); err != nil {
			return err
		}
		out = build(spec, st, true)
		return nil
	})
	return out, err
}
