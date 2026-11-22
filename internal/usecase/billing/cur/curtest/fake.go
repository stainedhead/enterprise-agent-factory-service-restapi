// Package curtest holds in-memory fakes of the CUR ingest ports.
package curtest

import (
	"context"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
)

// Fetcher serves canned lines per day and can fail.
type Fetcher struct {
	mu sync.Mutex
	// Lines maps a day (YYYY-MM-DD) to its lines.
	Lines map[string][]cur.Line
	// Errs maps a day to the errors its successive calls return before succeeding.
	Errs map[string][]error
	// Calls counts calls per day.
	Calls map[string]int
}

var _ cur.Fetcher = (*Fetcher)(nil)

// FetchDay implements cur.Fetcher.
func (f *Fetcher) FetchDay(_ context.Context, day time.Time) ([]cur.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := day.Format(time.DateOnly)
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[k]++
	if errs := f.Errs[k]; len(errs) > 0 {
		f.Errs[k] = errs[1:]
		if errs[0] != nil {
			return nil, errs[0]
		}
	}
	return f.Lines[k], nil
}

// Store is an in-memory cur.Store.
type Store struct {
	mu  sync.Mutex
	Dir cur.Directory
	// Days holds the rows last written per day.
	Days map[string][]cur.Row
	// Writes counts ReplaceDay calls per day.
	Writes map[string]int
	// ReplaceErr makes ReplaceDay fail.
	ReplaceErr error
}

var _ cur.Store = (*Store)(nil)

// Directory implements cur.Store.
func (s *Store) Directory(context.Context) (cur.Directory, error) { return s.Dir, nil }

// ReplaceDay implements cur.Store: the day's rows are replaced, other days are untouched.
func (s *Store) ReplaceDay(_ context.Context, day time.Time, rows []cur.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReplaceErr != nil {
		return s.ReplaceErr
	}
	if s.Days == nil {
		s.Days, s.Writes = map[string][]cur.Row{}, map[string]int{}
	}
	k := day.Format(time.DateOnly)
	s.Days[k] = append([]cur.Row(nil), rows...)
	s.Writes[k]++
	return nil
}
