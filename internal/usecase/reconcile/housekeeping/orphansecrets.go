package housekeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Orphan sweep settings (M2-C5, F-6).
const (
	// OrphanInterval is how often the sweep runs.
	OrphanInterval = 10 * time.Minute
	// OrphanGrace is how long a secret without a database row is left alone: a create in flight
	// has written Secrets Manager but not yet committed its row.
	OrphanGrace = time.Hour
	// OrphanRecoveryDays is the recovery window of a swept secret (SEC-1).
	OrphanRecoveryDays = 30
)

// ManagedSecret is a secret under the AFS path prefix as Secrets Manager lists it.
type ManagedSecret struct {
	ARN     string
	Name    string
	Created time.Time
	// Tagged is true when the secret carries afs-orphan=true: afs-api could not remove it after a
	// failed database write.
	Tagged bool
}

// SecretLister lists the secrets under a prefix.
type SecretLister interface {
	ListUnderPrefix(ctx context.Context, prefix string) ([]ManagedSecret, error)
}

// SecretScheduler schedules a secret's deletion with a recovery window.
type SecretScheduler interface {
	ScheduleDeletion(ctx context.Context, arn string, days int) error
}

// SecretRows is the database side of the sweep.
type SecretRows interface {
	// PathPrefix returns service setting secrets.path_prefix.
	PathPrefix(ctx context.Context) (string, error)
	// KnownARNs returns the subset of arns that has a row in afs.secrets.
	KnownARNs(ctx context.Context, arns []string) (map[string]bool, error)
	// RecordSwept writes the audit record of a swept secret.
	RecordSwept(ctx context.Context, arn, name, reason string) error
}

// OrphanSecrets is the sweep job: secrets under <prefix>/ that are tagged afs-orphan=true, or that
// have had no database row for OrphanGrace, are scheduled for deletion with the recovery window
// and audited. Secrets that have a row are never touched.
type OrphanSecrets struct {
	Lister    SecretLister
	Scheduler SecretScheduler
	Rows      SecretRows
	Clock     func() time.Time
	Log       *slog.Logger
	// Swept is called once per scheduled secret. Optional.
	Swept func()
}

// Run sweeps once. A failure on one secret is logged and does not stop the others; the first
// error is returned so the engine logs the run as failed.
func (o *OrphanSecrets) Run(ctx context.Context) error {
	now := time.Now
	if o.Clock != nil {
		now = o.Clock
	}
	prefix, err := o.Rows.PathPrefix(ctx)
	if err != nil {
		return fmt.Errorf("housekeeping: orphan sweep: %w", err)
	}
	listed, err := o.Lister.ListUnderPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("housekeeping: orphan sweep: list secrets: %w", err)
	}
	arns := make([]string, 0, len(listed))
	for _, s := range listed {
		arns = append(arns, s.ARN)
	}
	known := map[string]bool{}
	if len(arns) > 0 {
		if known, err = o.Rows.KnownARNs(ctx, arns); err != nil {
			return fmt.Errorf("housekeeping: orphan sweep: %w", err)
		}
	}
	var first error
	for _, s := range listed {
		reason := ""
		switch {
		case known[s.ARN]:
			continue
		case s.Tagged:
			reason = "tagged afs-orphan"
		case !s.Created.IsZero() && now().Sub(s.Created) >= OrphanGrace:
			reason = "no database row"
		default:
			continue
		}
		if err := o.sweep(ctx, s, reason); err != nil {
			if o.Log != nil {
				o.Log.ErrorContext(ctx, "orphan secret sweep failed", "name", s.Name, "err", err)
			}
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (o *OrphanSecrets) sweep(ctx context.Context, s ManagedSecret, reason string) error {
	if err := o.Scheduler.ScheduleDeletion(ctx, s.ARN, OrphanRecoveryDays); err != nil {
		return fmt.Errorf("schedule deletion of %s: %w", s.Name, err)
	}
	if o.Swept != nil {
		o.Swept()
	}
	if o.Log != nil {
		o.Log.InfoContext(ctx, "orphan secret scheduled for deletion", "name", s.Name, "reason", reason)
	}
	if err := o.Rows.RecordSwept(ctx, s.ARN, s.Name, reason); err != nil {
		return fmt.Errorf("audit sweep of %s: %w", s.Name, err)
	}
	return nil
}
