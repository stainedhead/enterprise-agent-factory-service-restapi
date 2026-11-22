// Package organizations reads the AWS Organizations account list for the hourly account mirror (M4-A1,
// TEAM-8, RC-8). It runs in the reconciler's account, which must be the organization's management
// account or a delegated administrator.
package organizations

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsorgs "github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// API is the subset of the Organizations SDK client this package calls.
type API interface {
	ListAccounts(ctx context.Context, in *awsorgs.ListAccountsInput, opts ...func(*awsorgs.Options)) (*awsorgs.ListAccountsOutput, error)
}

// Lister implements housekeeping.OrgLister.
type Lister struct{ api API }

var _ housekeeping.OrgLister = (*Lister)(nil)

// New returns a Lister on a configured SDK config.
func New(cfg aws.Config) *Lister { return &Lister{api: awsorgs.NewFromConfig(cfg)} }

// NewWithAPI returns a Lister on api (tests).
func NewWithAPI(api API) *Lister { return &Lister{api: api} }

// ListAccounts implements housekeeping.OrgLister: every account of the organization, all pages.
func (l *Lister) ListAccounts(ctx context.Context) ([]domain.OrgAccount, error) {
	var out []domain.OrgAccount
	var token *string
	for {
		page, err := l.api.ListAccounts(ctx, &awsorgs.ListAccountsInput{NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("organizations: ListAccounts: %w", err)
		}
		for _, a := range page.Accounts {
			id := aws.ToString(a.Id)
			if !domain.ValidAWSAccountID(id) {
				continue
			}
			out = append(out, domain.OrgAccount{ID: id, Name: aws.ToString(a.Name), Status: status(a)})
		}
		if page.NextToken == nil || *page.NextToken == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

// status maps the account's state. Organizations reports Status (ACTIVE, SUSPENDED, PENDING_CLOSURE)
// and, since 2024, State as well; anything but active never matches a team's filter, so an
// unknown value is treated as suspended.
func status(a types.Account) domain.OrgAccountStatus {
	s := string(a.State)
	if s == "" {
		s = string(a.Status) //nolint:staticcheck // Status is deprecated but still returned
	}
	switch s {
	case "ACTIVE":
		return domain.OrgAccountActive
	case "PENDING_CLOSURE":
		return domain.OrgAccountPendingClosure
	}
	return domain.OrgAccountSuspended
}
