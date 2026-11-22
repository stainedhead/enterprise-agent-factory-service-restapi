package organizations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsorgs "github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/organizations"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type fake struct {
	pages []awsorgs.ListAccountsOutput
	calls int
	err   error
}

func (f *fake) ListAccounts(_ context.Context, in *awsorgs.ListAccountsInput, _ ...func(*awsorgs.Options)) (*awsorgs.ListAccountsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	i := f.calls
	f.calls++
	if (i == 0) != (in.NextToken == nil) {
		return nil, errors.New("unexpected token")
	}
	return &f.pages[i], nil
}

func TestListAccountsPagesAndMapsStatus(t *testing.T) {
	f := &fake{pages: []awsorgs.ListAccountsOutput{
		{Accounts: []types.Account{{Id: aws.String("111122223333"), Name: aws.String("payments"), State: types.AccountStateActive}}, NextToken: aws.String("t")},
		{Accounts: []types.Account{
			{Id: aws.String("222233334444"), Name: aws.String("old"), State: types.AccountStatePendingClosure},
			{Id: aws.String("333344445555"), Name: aws.String("susp"), State: types.AccountStateSuspended},
			{Id: aws.String("bad"), Name: aws.String("ignored"), State: types.AccountStateActive},
		}},
	}}
	got, err := organizations.NewWithAPI(f).ListAccounts(t.Context())
	if err != nil || len(got) != 3 || f.calls != 2 {
		t.Fatalf("%+v %v calls=%d", got, err, f.calls)
	}
	want := []domain.OrgAccountStatus{domain.OrgAccountActive, domain.OrgAccountPendingClosure, domain.OrgAccountSuspended}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("account %d: %s", i, got[i].Status)
		}
	}
}

func TestListAccountsError(t *testing.T) {
	if _, err := organizations.NewWithAPI(&fake{err: errors.New("AccessDenied")}).ListAccounts(t.Context()); err == nil {
		t.Fatal("error expected")
	}
}
