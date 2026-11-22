package schedules_test

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func contextWithCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// cancellingStore cancels the context when a Put is attempted and fails it, as a shutdown during an
// upload would.
type cancellingStore struct {
	port.ObjectStore
	cancel context.CancelFunc
}

func (c cancellingStore) Put(ctx context.Context, o port.PutObject) (port.PutResult, error) {
	c.cancel()
	return c.ObjectStore.Put(ctx, o)
}

func (c cancellingStore) PresignGet(ctx context.Context, key string, d time.Duration) (port.PresignedURL, error) {
	return c.ObjectStore.PresignGet(ctx, key, d)
}
