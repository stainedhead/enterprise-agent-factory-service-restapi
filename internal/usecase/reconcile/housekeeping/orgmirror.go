package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// OrgMirrorInterval is how often the organization account mirror runs (RC-8, TEAM-8).
const OrgMirrorInterval = time.Hour

// ErrEmptyOrganization is returned when Organizations lists no account: the call is wrong (a missing
// grant answers with an error, not an empty list), so the mirror is left as it is rather than emptied.
var ErrEmptyOrganization = errors.New("housekeeping: Organizations returned no accounts; the mirror is left unchanged")

// OrgLister lists the accounts of the AWS organization.
type OrgLister interface {
	ListAccounts(ctx context.Context) ([]domain.OrgAccount, error)
}

// OrgResult says what a mirror run changed.
type OrgResult struct {
	Upserted int
	// Removed are the accounts no longer in the organization that were deleted from the mirror.
	Removed []string
	// Retired are accounts no longer in the organization that a role link still references; they stay,
	// marked suspended, so no filter matches them and the links are disabled on the next links pass.
	Retired []string
}

// OrgStore is the database side of the mirror (afs.org_aws_accounts).
type OrgStore interface {
	// Mirror makes the table equal to accounts: upsert every account with syncedAt, delete the ones not
	// listed, or mark them suspended when a role link references them.
	Mirror(ctx context.Context, accounts []domain.OrgAccount, syncedAt time.Time) (OrgResult, error)
}

// OrgMirror is the hourly job that mirrors the organization's accounts (M4-A1).
type OrgMirror struct {
	Lister OrgLister
	Store  OrgStore
	Clock  func() time.Time
	Log    *slog.Logger
}

// Run mirrors once. An account removed from the organization is gone (or retired) after this run.
func (m *OrgMirror) Run(ctx context.Context) error {
	accounts, err := m.Lister.ListAccounts(ctx)
	if err != nil {
		return fmt.Errorf("housekeeping: list organization accounts: %w", err)
	}
	if len(accounts) == 0 {
		return ErrEmptyOrganization
	}
	clock := m.Clock
	if clock == nil {
		clock = time.Now
	}
	res, err := m.Store.Mirror(ctx, accounts, clock().UTC())
	if err != nil {
		return fmt.Errorf("housekeeping: mirror organization accounts: %w", err)
	}
	if m.Log != nil {
		m.Log.InfoContext(ctx, "organization accounts mirrored", "accounts", len(accounts), "removed", len(res.Removed), "retired", len(res.Retired))
	}
	return nil
}

// OrgMirrorJob returns the scheduled job.
func OrgMirrorJob(m *OrgMirror) Job {
	return Job{Name: "org-account-mirror", Interval: OrgMirrorInterval, Run: m.Run}
}
