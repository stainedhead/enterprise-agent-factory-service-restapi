// Package eventbridge is the EventBridge adapter of the reconciler's outbox publisher.
package eventbridge

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/eventbridge"
)

// API is the subset of the EventBridge SDK client this package calls.
type API interface {
	PutEvents(ctx context.Context, in *eb.PutEventsInput, opts ...func(*eb.Options)) (*eb.PutEventsOutput, error)
}

// Client puts events on one event bus.
type Client struct {
	api API
	bus string
}

// New returns a Client for bus (AFS_EVENT_BUS_NAME).
func New(cfg aws.Config, bus string) *Client { return &Client{api: eb.NewFromConfig(cfg), bus: bus} }

// NewWithAPI returns a Client on api (tests).
func NewWithAPI(api API, bus string) *Client { return &Client{api: api, bus: bus} }
