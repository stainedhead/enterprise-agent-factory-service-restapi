// Package costtagstest holds in-memory fakes of the cost tag check's ports.
package costtagstest

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Billing serves canned activation states.
type Billing struct {
	Tags map[string]costtags.Activation
	Err  error
}

var _ costtags.Billing = (*Billing)(nil)

// ListTags implements costtags.Billing.
func (b *Billing) ListTags(context.Context) (map[string]costtags.Activation, error) {
	return b.Tags, b.Err
}

// Store is an in-memory costtags.Store whose rows persist across runs.
type Store struct {
	KeyList []string
	CUR     map[string]costtags.Seen
	Rows    map[string]costtags.Stored
	// FailSave makes Save fail.
	FailSave error
}

var _ costtags.Store = (*Store)(nil)

// Keys implements costtags.Store.
func (s *Store) Keys(context.Context, port.Tx) ([]string, error) { return s.KeyList, nil }

// SeenInCUR implements costtags.Store.
func (s *Store) SeenInCUR(context.Context, port.Tx, []string) (map[string]costtags.Seen, error) {
	return s.CUR, nil
}

// Stored implements costtags.Store.
func (s *Store) Stored(context.Context, port.Tx) (map[string]costtags.Stored, error) {
	out := map[string]costtags.Stored{}
	for k, v := range s.Rows {
		out[k] = v
	}
	return out, nil
}

// Save implements costtags.Store.
func (s *Store) Save(_ context.Context, _ port.Tx, rows []costtags.Row) error {
	if s.FailSave != nil {
		return s.FailSave
	}
	if s.Rows == nil {
		s.Rows = map[string]costtags.Stored{}
	}
	for _, r := range rows {
		s.Rows[r.Key] = costtags.Stored{First: r.First, Last: r.Last, AWSStatus: r.AWSStatus, CheckedAt: r.CheckedAt}
	}
	return nil
}

// Day returns midnight UTC of the given date.
func Day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
