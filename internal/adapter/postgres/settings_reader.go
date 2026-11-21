package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// SettingsReader implements port.SettingsReader on afs.service_settings. Every signed-in or anonymous
// caller may need a setting (CORS origins before authentication), so it reads in the lookup session;
// afs_app may read all settings (database contract §4).
type SettingsReader struct {
	runner *TxRunner
}

// NewSettingsReader returns a SettingsReader.
func NewSettingsReader(runner *TxRunner) *SettingsReader { return &SettingsReader{runner: runner} }

// Setting implements port.SettingsReader: the stored value of key, or its default when unset. An
// unknown key is a ValidationError.
func (r *SettingsReader) Setting(ctx context.Context, key string) (json.RawMessage, error) {
	spec, ok := domain.SettingSpecFor(key)
	if !ok {
		return nil, domain.Validation(domain.FieldError{Field: "key", Reason: "unknown setting " + key})
	}
	var raw []byte
	err := r.runner.ReadTx(ctx, lookupSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, `SELECT value FROM afs.service_settings WHERE key = $1`, key).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			raw = nil
			return nil
		}
		return err
	})
	if err != nil {
		return nil, MapError(err, OpRead, "setting")
	}
	if raw == nil {
		return json.RawMessage(spec.Default), nil
	}
	return json.RawMessage(raw), nil
}

var _ port.SettingsReader = (*SettingsReader)(nil)
