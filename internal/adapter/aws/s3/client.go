// Package s3 is the S3 adapter of afs-billing and the export bundle builder: it stores report, showback
// and export objects in the reports bucket with SSE-KMS and hands out pre-signed GET URLs (BI-4, RC-20).
// afs-api never links it (RA-US-14.5, archtest).
package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MaxPresignExpiry is the longest a SigV4 pre-signed URL can live (7 days). A URL signed with temporary
// credentials, such as EKS Pod Identity's, also stops working when those credentials expire.
const MaxPresignExpiry = 7 * 24 * time.Hour

// API is the subset of the S3 SDK client this package calls.
type API interface {
	PutObject(ctx context.Context, in *awss3.PutObjectInput, opts ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *awss3.ListObjectsV2Input, opts ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
}

// Presigner is the subset of the S3 presign client this package calls.
type Presigner interface {
	PresignGetObject(ctx context.Context, in *awss3.GetObjectInput, opts ...func(*awss3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// Store implements port.ObjectStore on one bucket.
type Store struct {
	api    API
	sign   Presigner
	bucket string
	kmsKey string
	now    func() time.Time
}

var _ port.ObjectStore = (*Store)(nil)

// Options configure a Store.
type Options struct {
	// Bucket is the reports bucket (AFS_REPORTS_BUCKET). Required.
	Bucket string
	// KMSKeyID is the KMS key that encrypts objects (AFS_REPORTS_KMS_KEY_ID). Empty uses the AWS managed
	// key for S3.
	KMSKeyID string
	// PathStyle addresses the bucket in the path (recorder and local endpoints).
	PathStyle bool
}

// New returns a Store on a configured SDK config.
func New(cfg aws.Config, o Options) (*Store, error) {
	if strings.TrimSpace(o.Bucket) == "" {
		return nil, errors.New("s3: the reports bucket is not configured (AFS_REPORTS_BUCKET)")
	}
	cl := awss3.NewFromConfig(cfg, func(so *awss3.Options) { so.UsePathStyle = o.PathStyle })
	return &Store{api: cl, sign: awss3.NewPresignClient(cl), bucket: o.Bucket, kmsKey: o.KMSKeyID, now: time.Now}, nil
}

// NewWithAPI returns a Store on api and a presigner (tests).
func NewWithAPI(api API, sign Presigner, o Options) *Store {
	return &Store{api: api, sign: sign, bucket: o.Bucket, kmsKey: o.KMSKeyID, now: time.Now}
}

func (s *Store) uri(key string) string { return fmt.Sprintf("s3://%s/%s", s.bucket, key) }
