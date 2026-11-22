package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	awss3 "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/s3"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

// exportBlob returns the object store that holds export bundles: an S3 bucket (AFS_EXPORT_BUCKET) with
// server-side KMS encryption under AFS_EXPORT_KMS_KEY_ID, on the S3 adapter shared with afs-billing
// (adapter/aws/s3). With no bucket it returns nil and the export builder stays unregistered, so a pending
// export is not claimed by a reconciler that cannot write it. A bucket without a key is a configuration
// error: bundles are never written with the bucket's default encryption. pathStyle addresses the bucket
// in the path (a custom AWS endpoint).
func exportBlob(cfg awssdk.Config, env func(string) string, pathStyle bool) (export.Blob, error) {
	bucket := env("AFS_EXPORT_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	key := env("AFS_EXPORT_KMS_KEY_ID")
	if key == "" {
		return nil, errors.New("AFS_EXPORT_KMS_KEY_ID is required with AFS_EXPORT_BUCKET: bundles are encrypted with SSE-KMS")
	}
	st, err := awss3.New(cfg, awss3.Options{Bucket: bucket, KMSKeyID: key, PathStyle: pathStyle})
	if err != nil {
		return nil, err
	}
	return storeBlob{st}, nil
}

// storeBlob adapts a port.ObjectStore to export.Blob.
type storeBlob struct{ store port.ObjectStore }

func (b storeBlob) Put(ctx context.Context, key string, body []byte) (string, error) {
	res, err := b.store.Put(ctx, port.PutObject{Key: key, Body: body, ContentType: "application/json"})
	return res.URI, err
}

func (b storeBlob) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	u, err := b.store.PresignGet(ctx, key, ttl)
	return u.URL, u.ExpiresAt, err
}

// wireExport registers the export bundle builder (M4-F3) as a leader-only job. It does nothing without a
// blob store.
func wireExport(e export.Scheduler, runner *postgres.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen, blob export.Blob, now func() time.Time, log *slog.Logger) bool {
	if blob == nil {
		log.Info("export bundles disabled: no export bucket configured")
		return false
	}
	b := &export.Builder{Store: postgres.NewExportJobs(runner, audit, outbox, ids), Blob: blob, Clock: now, Log: log}
	b.Register(e)
	return true
}
