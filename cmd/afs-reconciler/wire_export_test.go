package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

type fakeSched struct{ names []string }

func (f *fakeSched) Every(name string, _ time.Duration, _ func(context.Context) error) {
	f.names = append(f.names, name)
}

type noBlob struct{}

func (noBlob) Put(context.Context, string, []byte) (string, error) { return "", nil }
func (noBlob) PresignGet(context.Context, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func TestWireExport(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		blob export.Blob
		want int
	}{
		{"no bucket registers nothing", nil, 0},
		{"a blob store registers the builder", noBlob{}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSched{}
			got := wireExport(s, nil, nil, nil, nil, tc.blob, time.Now, log)
			if got != (tc.want == 1) || len(s.names) != tc.want {
				t.Fatalf("registered = %v %v", got, s.names)
			}
		})
	}
	if exportBlob() != nil {
		t.Fatal("the S3 adapter is not wired yet")
	}
}
