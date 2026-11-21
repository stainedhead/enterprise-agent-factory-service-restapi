// Package secretsmanager is the Secrets Manager adapter. afs-api uses it only to write secret
// values; the reconciler uses it to schedule deletions (delete.go). Callers never receive a secret
// value from this package.
package secretsmanager

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// API is the subset of the Secrets Manager SDK client this package calls; *sm.Client satisfies it
// and tests inject a fake.
type API interface {
	DeleteSecret(ctx context.Context, in *sm.DeleteSecretInput, opts ...func(*sm.Options)) (*sm.DeleteSecretOutput, error)
	ListSecrets(ctx context.Context, in *sm.ListSecretsInput, opts ...func(*sm.Options)) (*sm.ListSecretsOutput, error)
}

// Client wraps API.
type Client struct {
	api API
}

// New returns a Client on a configured SDK config (region and credentials from the environment, see
// aws/config.go).
func New(cfg aws.Config) *Client { return &Client{api: sm.NewFromConfig(cfg)} }

// NewWithAPI returns a Client on api (tests, or an endpoint recorder).
func NewWithAPI(api API) *Client { return &Client{api: api} }
