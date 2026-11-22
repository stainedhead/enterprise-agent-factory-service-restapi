package housekeeping

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSM struct {
	list      []ManagedSecret
	listErr   error
	scheduled []string
	schedErr  map[string]error
	days      int
}

func (f *fakeSM) ListUnderPrefix(context.Context, string) ([]ManagedSecret, error) {
	return f.list, f.listErr
}

func (f *fakeSM) ScheduleDeletion(_ context.Context, arn string, days int) error {
	if err := f.schedErr[arn]; err != nil {
		return err
	}
	f.scheduled, f.days = append(f.scheduled, arn), days
	return nil
}

type fakeRows struct {
	known    map[string]bool
	audited  []string
	prefix   string
	prefErr  error
	knownErr error
	auditErr error
	asked    int
}

func (f *fakeRows) PathPrefix(context.Context) (string, error) { return f.prefix, f.prefErr }
func (f *fakeRows) KnownARNs(_ context.Context, arns []string) (map[string]bool, error) {
	f.asked = len(arns)
	return f.known, f.knownErr
}
func (f *fakeRows) RecordSwept(_ context.Context, arn, _, reason string) error {
	f.audited = append(f.audited, arn+"|"+reason)
	return f.auditErr
}

func TestOrphanSweep(t *testing.T) {
	now := time.Now()
	old, fresh := now.Add(-2*time.Hour), now.Add(-5*time.Minute)
	list := []ManagedSecret{
		{ARN: "a-known-old", Created: old},                    // has a row: never touched
		{ARN: "b-orphan-old", Created: old},                   // no row for 2 h: swept
		{ARN: "c-orphan-fresh", Created: fresh},               // create may be in flight: left alone
		{ARN: "d-tagged-fresh", Created: fresh, Tagged: true}, // tagged: swept at once
		{ARN: "e-known-tagged", Created: old, Tagged: true},   // a row exists: never touched
		{ARN: "f-no-date"},                                    // unknown age: left alone
	}
	sm := &fakeSM{list: list}
	rows := &fakeRows{known: map[string]bool{"a-known-old": true, "e-known-tagged": true}, prefix: "afs"}
	o := &OrphanSecrets{Lister: sm, Scheduler: sm, Rows: rows, Clock: func() time.Time { return now }}
	swept := 0
	o.Swept = func() { swept++ }
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sm.scheduled, ","); got != "b-orphan-old,d-tagged-fresh" || sm.days != 30 || swept != 2 {
		t.Errorf("scheduled %s days=%d swept=%d", got, sm.days, swept)
	}
	if got := strings.Join(rows.audited, ","); got != "b-orphan-old|no database row,d-tagged-fresh|tagged afs-orphan" {
		t.Errorf("audited %s", got)
	}
	// Idempotent: a second run over the same list (Secrets Manager no longer lists swept secrets) does nothing more.
	sm.list = []ManagedSecret{list[0], list[2]}
	sm.scheduled = nil
	if err := o.Run(t.Context()); err != nil || len(sm.scheduled) != 0 {
		t.Errorf("second run: %v %v", err, sm.scheduled)
	}
}

func TestOrphanSweepFailures(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	boom := errors.New("boom")
	tests := []struct {
		name string
		sm   *fakeSM
		rows *fakeRows
		want []string
	}{
		{"prefix", &fakeSM{}, &fakeRows{prefErr: boom}, nil},
		{"list", &fakeSM{listErr: boom}, &fakeRows{}, nil},
		{"known", &fakeSM{list: []ManagedSecret{{ARN: "x", Created: old}}}, &fakeRows{knownErr: boom}, nil},
		{"one failure does not stop the rest", &fakeSM{list: []ManagedSecret{{ARN: "x", Created: old}, {ARN: "y", Created: old}},
			schedErr: map[string]error{"x": boom}}, &fakeRows{known: map[string]bool{}}, []string{"y"}},
		{"audit failure is reported", &fakeSM{list: []ManagedSecret{{ARN: "x", Created: old}}}, &fakeRows{known: map[string]bool{}, auditErr: boom}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &OrphanSecrets{Lister: tt.sm, Scheduler: tt.sm, Rows: tt.rows, Clock: func() time.Time { return now }}
			if err := o.Run(t.Context()); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if strings.Join(tt.sm.scheduled, ",") != strings.Join(tt.want, ",") {
				t.Errorf("scheduled %v, want %v", tt.sm.scheduled, tt.want)
			}
		})
	}
	// An empty listing asks the database nothing.
	rows := &fakeRows{}
	if err := (&OrphanSecrets{Lister: &fakeSM{}, Scheduler: &fakeSM{}, Rows: rows}).Run(t.Context()); err != nil || rows.asked != 0 {
		t.Errorf("empty: %v asked=%d", err, rows.asked)
	}
}
