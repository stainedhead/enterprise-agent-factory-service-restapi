// Package showbacktest is an in-memory showback.Source.
package showbacktest

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Source implements showback.Source with fixed answers.
type Source struct {
	Total       showback.Measures
	CostCenters []showback.Line
	Tags        map[string][]showback.Line
	Err         error
	// Spans lists the first and last day of every call.
	Spans [][2]time.Time
	// TagKeys lists the keys asked for.
	TagKeys []string
}

var _ showback.Source = (*Source)(nil)

// Totals implements showback.Source.
func (s *Source) Totals(_ context.Context, _ port.Tx, first, last time.Time) (showback.Measures, error) {
	s.Spans = append(s.Spans, [2]time.Time{first, last})
	return s.Total, s.Err
}

// ByCostCenter implements showback.Source.
func (s *Source) ByCostCenter(context.Context, port.Tx, time.Time, time.Time) ([]showback.Line, error) {
	return s.CostCenters, s.Err
}

// ByTag implements showback.Source.
func (s *Source) ByTag(_ context.Context, _ port.Tx, _, _ time.Time, key string) ([]showback.Line, error) {
	s.TagKeys = append(s.TagKeys, key)
	return s.Tags[key], s.Err
}
