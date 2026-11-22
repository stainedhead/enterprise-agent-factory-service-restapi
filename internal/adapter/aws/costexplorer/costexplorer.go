// Package costexplorer reads which user-defined cost allocation tags are active in AWS Billing (BI-6):
// Cost Explorer's ListCostAllocationTags, paginated, as the map afs-billing's tag check compares with
// what the Cost and Usage Report shows.
package costexplorer

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
)

// API is the part of the Cost Explorer client the adapter uses.
type API interface {
	ListCostAllocationTags(ctx context.Context, in *ce.ListCostAllocationTagsInput, opts ...func(*ce.Options)) (*ce.ListCostAllocationTagsOutput, error)
}

// maxPages bounds the pagination.
const maxPages = 100

// Billing implements costtags.Billing on Cost Explorer.
type Billing struct{ api API }

var _ costtags.Billing = (*Billing)(nil)

// New returns a Billing on the SDK configuration. Cost Explorer is a global service served from
// us-east-1, so awsCfg should name it.
func New(awsCfg awssdk.Config) *Billing { return &Billing{api: ce.NewFromConfig(awsCfg)} }

// NewWithAPI returns a Billing on api (for tests).
func NewWithAPI(api API) *Billing { return &Billing{api: api} }

// ListTags implements costtags.Billing: the user-defined tags AWS knows, by key.
func (b *Billing) ListTags(ctx context.Context) (map[string]costtags.Activation, error) {
	out := map[string]costtags.Activation{}
	var token *string
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("costexplorer: more than %d pages of cost allocation tags", maxPages)
		}
		res, err := b.api.ListCostAllocationTags(ctx, &ce.ListCostAllocationTagsInput{
			NextToken: token, Type: types.CostAllocationTagTypeUserDefined, MaxResults: awssdk.Int32(100),
		})
		if err != nil {
			return nil, fmt.Errorf("list cost allocation tags: %w", err)
		}
		for _, t := range res.CostAllocationTags {
			if t.TagKey == nil {
				continue
			}
			a := costtags.Activation{Active: t.Status == types.CostAllocationTagStatusActive}
			if t.LastUpdatedDate != nil {
				a.Since = parseDate(*t.LastUpdatedDate)
			}
			out[*t.TagKey] = a
		}
		if res.NextToken == nil || *res.NextToken == "" {
			return out, nil
		}
		token = res.NextToken
	}
}

// parseDate reads the date AWS reports (an ISO-8601 date or date-time); zero when it is none of those.
func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "2006-01"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
