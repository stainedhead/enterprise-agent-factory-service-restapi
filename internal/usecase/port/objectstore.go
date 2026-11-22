package port

import (
	"context"
	"time"
)

// PutObject is an object to store. The store encrypts it with the platform KMS key (SSE-KMS).
type PutObject struct {
	// Key is the object key inside the store's bucket.
	Key         string
	Body        []byte
	ContentType string
}

// PutResult says where an object was stored.
type PutResult struct {
	// URI is s3://<bucket>/<key>.
	URI  string
	Size int64
}

// PresignedURL is a time-limited GET URL for an object.
type PresignedURL struct {
	URL       string
	ExpiresAt time.Time
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ObjectStore is the reports bucket (AFS_REPORTS_BUCKET): afs-billing writes report and showback
// CSVs to it, and the export bundle builder writes bundles (RC-20). One store addresses one bucket.
// afs-api never uses it (RA-US-14.5).
type ObjectStore interface {
	// Put stores body at key, encrypted with SSE-KMS, replacing an object of the same key.
	Put(ctx context.Context, o PutObject) (PutResult, error)
	// PresignGet returns a GET URL for key that stops working after expires.
	PresignGet(ctx context.Context, key string, expires time.Duration) (PresignedURL, error)
	// List returns the objects whose key starts with prefix, in key order.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}
