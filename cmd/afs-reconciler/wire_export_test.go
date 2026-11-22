package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"

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
}

func exportEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestExportBlobFromEnvironment(t *testing.T) {
	srv := awstest.New(t)
	cfg := srv.Config()
	if b, err := exportBlob(cfg, exportEnv(nil), true); b != nil || err != nil {
		t.Fatalf("no bucket = %v, %v; want nil and no error", b, err)
	}
	if _, err := exportBlob(cfg, exportEnv(map[string]string{"AFS_EXPORT_BUCKET": "example-exports"}), true); err == nil || !strings.Contains(err.Error(), "AFS_EXPORT_KMS_KEY_ID") {
		t.Fatalf("a bucket without a KMS key must be refused, got %v", err)
	}
	const kms = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
	blob, err := exportBlob(cfg, exportEnv(map[string]string{"AFS_EXPORT_BUCKET": "example-exports", "AFS_EXPORT_KMS_KEY_ID": kms}), true)
	if err != nil || blob == nil {
		t.Fatalf("exportBlob = %v, %v", blob, err)
	}
	// The builder is registered whenever the blob store is.
	s := &fakeSched{}
	if !wireExport(s, nil, nil, nil, nil, blob, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil))) || len(s.names) != 1 {
		t.Fatalf("the builder was not registered: %v", s.names)
	}

	srv.OnAction("PUT /example-exports/exports/exp_1.json", awstest.Response{Status: 200, ContentType: "application/xml"})
	uri, err := blob.Put(t.Context(), "exports/exp_1.json", []byte(`{"v":1}`))
	if err != nil || uri != "s3://example-exports/exports/exp_1.json" {
		t.Fatalf("Put = %q, %v", uri, err)
	}
	r := srv.Requests()[0]
	if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != kms {
		t.Errorf("bundle not written with SSE-KMS: %v", r.Header)
	}
	url, exp, err := blob.PresignGet(t.Context(), "exports/exp_1.json", time.Hour)
	if err != nil || !strings.Contains(url, "example-exports") || !strings.Contains(url, "X-Amz-Signature") || exp.IsZero() {
		t.Fatalf("PresignGet = %q, %v, %v", url, exp, err)
	}
}
