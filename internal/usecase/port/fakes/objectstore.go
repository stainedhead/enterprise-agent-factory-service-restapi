package fakes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ObjectStore is an in-memory port.ObjectStore.
type ObjectStore struct {
	mu sync.Mutex
	// Bucket names the fake bucket in URIs ("example-reports" by default).
	Bucket string
	// Clock stamps objects and pre-signed URLs; time.Now when nil.
	Clock *Clock
	// Objects holds the stored bodies by key; Types holds their content types.
	Objects map[string][]byte
	Types   map[string]string
	// Puts counts Put calls, including replacements.
	Puts int
	// PutErr, PresignErr and ListErr are returned by the matching call when set.
	PutErr, PresignErr, ListErr error
	mod                         map[string]time.Time
}

var _ port.ObjectStore = (*ObjectStore)(nil)

// ErrNoObject is returned by Body for a key that is not stored.
var ErrNoObject = errors.New("fakes: no such object")

func (s *ObjectStore) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *ObjectStore) bucket() string {
	if s.Bucket == "" {
		return "example-reports"
	}
	return s.Bucket
}

// Put implements port.ObjectStore.
func (s *ObjectStore) Put(_ context.Context, o port.PutObject) (port.PutResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PutErr != nil {
		return port.PutResult{}, s.PutErr
	}
	if s.Objects == nil {
		s.Objects, s.Types, s.mod = map[string][]byte{}, map[string]string{}, map[string]time.Time{}
	}
	s.Objects[o.Key] = append([]byte(nil), o.Body...)
	s.Types[o.Key] = o.ContentType
	s.mod[o.Key] = s.now()
	s.Puts++
	return port.PutResult{URI: "s3://" + s.bucket() + "/" + o.Key, Size: int64(len(o.Body))}, nil
}

// PresignGet implements port.ObjectStore. The URL names the key and the expiry so tests can tell
// them apart.
func (s *ObjectStore) PresignGet(_ context.Context, key string, expires time.Duration) (port.PresignedURL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PresignErr != nil {
		return port.PresignedURL{}, s.PresignErr
	}
	if _, ok := s.Objects[key]; !ok {
		return port.PresignedURL{}, ErrNoObject
	}
	return port.PresignedURL{URL: fmt.Sprintf("https://%s.s3.example.com/%s?X-Amz-Expires=%d&X-Amz-Signature=fake", s.bucket(), key, int64(expires.Seconds())),
		ExpiresAt: s.now().Add(expires).UTC()}, nil
}

// List implements port.ObjectStore.
func (s *ObjectStore) List(_ context.Context, prefix string) ([]port.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	var out []port.ObjectInfo
	for k, b := range s.Objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, port.ObjectInfo{Key: k, Size: int64(len(b)), LastModified: s.mod[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Body returns the stored bytes of key.
func (s *ObjectStore) Body(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Objects[key]
	if !ok {
		return nil, ErrNoObject
	}
	return b, nil
}
