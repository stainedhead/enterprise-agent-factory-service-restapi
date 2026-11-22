// Package sts assumes the team's Bedrock role for afs-gateway (gateway contract §1, M3-A5). The
// gateway's own role is the only principal those roles trust (RC-1), so assuming one is how a call
// acquires the team's identity and Bedrock permission, and only for the team's inference profiles.
package sts

import (
	"context"
	"errors"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
)

// API is the part of the STS client the adapter uses.
type API interface {
	AssumeRole(ctx context.Context, in *awssts.AssumeRoleInput, opts ...func(*awssts.Options)) (*awssts.AssumeRoleOutput, error)
}

// Defaults.
const (
	// SessionName is the role session name of every assumption; CloudTrail shows it.
	SessionName = "afs-gateway"
	// SessionDuration is the lifetime requested for assumed credentials.
	SessionDuration = time.Hour
	// RefreshBefore is how long before expiry credentials are replaced.
	RefreshBefore = 5 * time.Minute
)

// Assumer hands out credentials for a team's Bedrock role and caches them until shortly before they
// expire. It is safe for concurrent use; concurrent calls for one role share a single AssumeRole.
type Assumer struct {
	api API
	now func() time.Time

	mu     sync.Mutex
	creds  map[string]awssdk.Credentials
	flying map[string]*flight
}

type flight struct {
	done  chan struct{}
	creds awssdk.Credentials
	err   error
}

// NewAssumer returns an Assumer on the gateway's own AWS configuration.
func NewAssumer(cfg awssdk.Config) *Assumer { return newAssumer(awssts.NewFromConfig(cfg), nil) }

func newAssumer(api API, now func() time.Time) *Assumer {
	if now == nil {
		now = time.Now
	}
	return &Assumer{api: api, now: now, creds: map[string]awssdk.Credentials{}, flying: map[string]*flight{}}
}

// ErrNoRole is returned for an empty role ARN.
var ErrNoRole = errors.New("sts: no role to assume")
