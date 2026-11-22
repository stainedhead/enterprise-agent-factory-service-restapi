package s3

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// PresignGet implements port.ObjectStore. Nothing is sent to S3: the URL is signed locally, so it is
// valid for objects that are written later, and it stops working after expires (at most 7 days).
func (s *Store) PresignGet(ctx context.Context, key string, expires time.Duration) (port.PresignedURL, error) {
	if err := checkKey(key); err != nil {
		return port.PresignedURL{}, err
	}
	if expires <= 0 || expires > MaxPresignExpiry {
		return port.PresignedURL{}, fmt.Errorf("s3: a pre-signed URL expires in 1 second to 7 days, not %s", expires)
	}
	signed := s.now().UTC()
	req, err := s.sign.PresignGetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)},
		awss3.WithPresignExpires(expires))
	if err != nil {
		return port.PresignedURL{}, fmt.Errorf("s3: presign %s: %w", s.uri(key), err)
	}
	return port.PresignedURL{URL: req.URL, ExpiresAt: signed.Add(expires)}, nil
}
