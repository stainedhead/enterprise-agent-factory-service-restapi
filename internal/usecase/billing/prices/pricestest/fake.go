// Package pricestest holds in-memory fakes of the price sync ports.
package pricestest

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// List serves a canned price list.
type List struct {
	Prices []prices.ListPrice
	Err    error
	Calls  int
}

var _ prices.PriceList = (*List)(nil)

// Fetch implements prices.PriceList.
func (l *List) Fetch(context.Context) ([]prices.ListPrice, error) {
	l.Calls++
	return l.Prices, l.Err
}

// Store is an in-memory prices.Store.
type Store struct {
	Enabled    bool
	EnabledErr error
	Models_    []prices.ModelState
	// Inserted and Superseded record the writes in order.
	Inserted   []prices.Proposal
	Superseded []string
	// FailInsert makes Insert fail.
	FailInsert error
}

var _ prices.Store = (*Store)(nil)

// SyncEnabled implements prices.Store.
func (s *Store) SyncEnabled(context.Context) (bool, error) { return s.Enabled, s.EnabledErr }

// Models implements prices.Store.
func (s *Store) Models(context.Context, port.Tx) ([]prices.ModelState, error) { return s.Models_, nil }

// Supersede implements prices.Store.
func (s *Store) Supersede(_ context.Context, _ port.Tx, id string) error {
	s.Superseded = append(s.Superseded, id)
	return nil
}

// Insert implements prices.Store.
func (s *Store) Insert(_ context.Context, _ port.Tx, p prices.Proposal) error {
	if s.FailInsert != nil {
		return s.FailInsert
	}
	s.Inserted = append(s.Inserted, p)
	return nil
}
