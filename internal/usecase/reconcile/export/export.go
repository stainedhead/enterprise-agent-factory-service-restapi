// Package export is the reconciler's export bundle builder (RC-20): it takes the pending export
// requests afs-api recorded, reads the desired state, writes a KMS-encrypted bundle to S3 and records
// its checksum, size and a 7-day pre-signed link on the request.
package export

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Interval is how often the builder looks for pending requests.
const Interval = 15 * time.Second

// Request is a claimed export request.
type Request struct {
	ID string
	// TeamIDs are the requested teams; empty means every team.
	TeamIDs []string
}

// Result is what a finished build records on the request.
type Result struct {
	SHA256      string
	SizeBytes   int64
	S3URI       string
	DownloadURL string
	ExpiresAt   time.Time
	Teams       int
}

// Store is the database side of the builder.
type Store interface {
	// Claim moves the oldest pending request to running and returns it; nil when none is pending.
	Claim(ctx context.Context) (*Request, error)
	// Build reads the desired state of the requested teams into a bundle.
	Build(ctx context.Context, teamIDs []string, now time.Time) (domain.Bundle, error)
	// Succeed records the result, audits it and publishes export.completed in one transaction.
	Succeed(ctx context.Context, id string, r Result) error
	// Fail records the failure summary (no checksum), audits it and publishes export.failed.
	Fail(ctx context.Context, id, summary string) error
}

// Blob is the object store for bundles. Put writes with server-side encryption under the export KMS
// key and returns the object's s3:// URI; PresignGet returns a GET link valid for ttl.
type Blob interface {
	Put(ctx context.Context, key string, body []byte) (uri string, err error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, expires time.Time, err error)
}

// Scheduler is the engine's leader-only job scheduler.
type Scheduler interface {
	Every(name string, interval time.Duration, fn func(ctx context.Context) error)
}

// Builder builds pending exports.
type Builder struct {
	Store Store
	Blob  Blob
	Clock func() time.Time
	Log   *slog.Logger
}

// Register adds the builder to the scheduler.
func (b *Builder) Register(s Scheduler) { s.Every("export-bundles", Interval, b.Run) }

// Key is the object key of an export's bundle.
func Key(id string) string { return "exports/" + id + ".json" }

// Run builds every pending export. A failed build is recorded on its request and does not stop the
// others; an error of the store itself is returned.
func (b *Builder) Run(ctx context.Context) error {
	for {
		req, err := b.Store.Claim(ctx)
		if err != nil {
			return fmt.Errorf("export: claim: %w", err)
		}
		if req == nil {
			return nil
		}
		res, step, err := b.build(ctx, req)
		if err != nil {
			b.log().WarnContext(ctx, "export failed", "export", req.ID, "step", step, "error", err)
			// The summary names the step only; the cause stays in the log.
			if ferr := b.Store.Fail(ctx, req.ID, step+" failed"); ferr != nil {
				return fmt.Errorf("export: record failure of %s: %w", req.ID, ferr)
			}
			continue
		}
		if err := b.Store.Succeed(ctx, req.ID, res); err != nil {
			return fmt.Errorf("export: record result of %s: %w", req.ID, err)
		}
	}
}

func (b *Builder) build(ctx context.Context, req *Request) (Result, string, error) {
	now := b.now()
	bundle, err := b.Store.Build(ctx, req.TeamIDs, now)
	if err != nil {
		return Result{}, "reading desired state", err
	}
	sum, doc, err := bundle.Seal()
	if err != nil {
		return Result{}, "sealing the bundle", err
	}
	uri, err := b.Blob.Put(ctx, Key(req.ID), doc)
	if err != nil {
		return Result{}, "writing the bundle", err
	}
	url, exp, err := b.Blob.PresignGet(ctx, Key(req.ID), domain.ExportRetention)
	if err != nil {
		return Result{}, "signing the download link", err
	}
	return Result{SHA256: sum, SizeBytes: int64(len(doc)), S3URI: uri, DownloadURL: url, ExpiresAt: exp, Teams: len(bundle.Teams)}, "", nil
}

func (b *Builder) now() time.Time {
	if b.Clock != nil {
		return b.Clock().UTC()
	}
	return time.Now().UTC()
}

func (b *Builder) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}
