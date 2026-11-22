package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Put implements port.ObjectStore: the object is encrypted with SSE-KMS and replaces an object of the
// same key.
func (s *Store) Put(ctx context.Context, o port.PutObject) (port.PutResult, error) {
	if err := checkKey(o.Key); err != nil {
		return port.PutResult{}, err
	}
	in := &awss3.PutObjectInput{
		Bucket:               aws.String(s.bucket),
		Key:                  aws.String(o.Key),
		Body:                 bytes.NewReader(o.Body),
		ContentLength:        aws.Int64(int64(len(o.Body))),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms,
	}
	if o.ContentType != "" {
		in.ContentType = aws.String(o.ContentType)
	}
	if s.kmsKey != "" {
		in.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	if _, err := s.api.PutObject(ctx, in); err != nil {
		return port.PutResult{}, fmt.Errorf("s3: PutObject %s: %w", s.uri(o.Key), err)
	}
	return port.PutResult{URI: s.uri(o.Key), Size: int64(len(o.Body))}, nil
}

// List implements port.ObjectStore: every object whose key starts with prefix, following pages.
func (s *Store) List(ctx context.Context, prefix string) ([]port.ObjectInfo, error) {
	out := []port.ObjectInfo{}
	var token *string
	for {
		res, err := s.api.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("s3: ListObjectsV2 %s: %w", s.uri(prefix), err)
		}
		for _, o := range res.Contents {
			info := port.ObjectInfo{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				info.LastModified = o.LastModified.UTC()
			}
			out = append(out, info)
		}
		if !aws.ToBool(res.IsTruncated) || res.NextContinuationToken == nil {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

func checkKey(key string) error {
	switch {
	case key == "":
		return errors.New("s3: an object key is required")
	case strings.HasPrefix(key, "/"), strings.Contains(key, ".."):
		return fmt.Errorf("s3: invalid object key %q", key)
	case len(key) > 1024:
		return errors.New("s3: object key longer than 1024 bytes")
	}
	return nil
}
