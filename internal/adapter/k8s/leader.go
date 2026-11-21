package k8s

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Default election timings: a crashed leader's lease expires after 15 s and a replica takes over
// within one retry period after that, well under the 30 s failover target.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// ElectorConfig configures leader election on a coordination.k8s.io Lease.
type ElectorConfig struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Identity is unique per replica (the pod name).
	Identity string
	// LeaseDuration, RenewDeadline and RetryPeriod default to the constants above.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	// ReleaseOnCancel gives the lease up when Run's context ends, so a rolling update hands over at once.
	ReleaseOnCancel bool
}

// Elector runs leader election and reports whether this replica leads.
type Elector struct {
	cfg     ElectorConfig
	leading atomic.Bool
}

// NewElector validates cfg.
func NewElector(cfg ElectorConfig) (*Elector, error) {
	if cfg.Client == nil || cfg.Namespace == "" || cfg.Name == "" || cfg.Identity == "" {
		return nil, errors.New("k8s: elector needs Client, Namespace, Name and Identity")
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = DefaultLeaseDuration
	}
	if cfg.RenewDeadline == 0 {
		cfg.RenewDeadline = DefaultRenewDeadline
	}
	if cfg.RetryPeriod == 0 {
		cfg.RetryPeriod = DefaultRetryPeriod
	}
	return &Elector{cfg: cfg}, nil
}

// IsLeader reports whether this replica currently holds the lease (the readiness endpoint shows it).
func (e *Elector) IsLeader() bool { return e.leading.Load() }

// Identity returns this replica's identity.
func (e *Elector) Identity() string { return e.cfg.Identity }

// Run campaigns until ctx ends. Each time the replica wins, lead runs with a context that is
// cancelled when leadership is lost; Run then campaigns again.
func (e *Elector) Run(ctx context.Context, lead func(ctx context.Context)) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: e.cfg.Name, Namespace: e.cfg.Namespace},
		Client:     e.cfg.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: e.cfg.Identity},
	}
	for ctx.Err() == nil {
		le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock: lock, LeaseDuration: e.cfg.LeaseDuration, RenewDeadline: e.cfg.RenewDeadline, RetryPeriod: e.cfg.RetryPeriod,
			ReleaseOnCancel: e.cfg.ReleaseOnCancel, Name: e.cfg.Name,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
					e.leading.Store(true)
					lead(lctx)
				},
				OnStoppedLeading: func() { e.leading.Store(false) },
			},
		})
		if err != nil {
			return fmt.Errorf("k8s: leader election: %w", err)
		}
		le.Run(ctx)
		e.leading.Store(false)
	}
	return nil
}
