// Package wafv2 reads and replaces the addresses of the two WAF IP sets that hold the collaboration source
// allow-list (RC-28). The reconciler's role may call GetIPSet and UpdateIPSet on those two ARNs only.
package wafv2

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awswaf "github.com/aws/aws-sdk-go-v2/service/wafv2"
	"github.com/aws/aws-sdk-go-v2/service/wafv2/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// API is the subset of the WAFv2 SDK client this package calls.
type API interface {
	GetIPSet(ctx context.Context, in *awswaf.GetIPSetInput, opts ...func(*awswaf.Options)) (*awswaf.GetIPSetOutput, error)
	UpdateIPSet(ctx context.Context, in *awswaf.UpdateIPSetInput, opts ...func(*awswaf.Options)) (*awswaf.UpdateIPSetOutput, error)
}

// Sets implements collab.IPSets.
type Sets struct{ api API }

var _ collab.IPSets = (*Sets)(nil)

// New returns Sets on a configured SDK config.
func New(cfg aws.Config) *Sets { return &Sets{api: awswaf.NewFromConfig(cfg)} }

// NewWithAPI returns Sets on api (tests).
func NewWithAPI(api API) *Sets { return &Sets{api: api} }

// parts splits an IP set ARN, arn:<partition>:wafv2:<region>:<account>:<scope>/ipset/<name>/<id>, into the
// scope, name and id the API needs.
func parts(arn string) (types.Scope, string, string, error) {
	f := strings.Split(arn, ":")
	if len(f) != 6 || f[2] != "wafv2" {
		return "", "", "", fmt.Errorf("wafv2: %q is not a WAFv2 ARN", arn)
	}
	r := strings.Split(f[5], "/")
	if len(r) != 4 || r[1] != "ipset" || r[2] == "" || r[3] == "" {
		return "", "", "", fmt.Errorf("wafv2: %q is not an IP set ARN", arn)
	}
	switch r[0] {
	case "regional":
		return types.ScopeRegional, r[2], r[3], nil
	case "global":
		return types.ScopeCloudfront, r[2], r[3], nil
	}
	return "", "", "", fmt.Errorf("wafv2: unknown scope %q in %q", r[0], arn)
}

// Addresses implements collab.IPSets.
func (s *Sets) Addresses(ctx context.Context, ref collab.IPSetRef) ([]string, error) {
	scope, name, id, err := parts(ref.ARN)
	if err != nil {
		return nil, err
	}
	out, err := s.api.GetIPSet(ctx, &awswaf.GetIPSetInput{Scope: scope, Name: &name, Id: &id})
	if err != nil {
		return nil, fmt.Errorf("wafv2: GetIPSet: %w", err)
	}
	if out.IPSet == nil {
		return nil, nil
	}
	return append([]string(nil), out.IPSet.Addresses...), nil
}

// Update implements collab.IPSets. The lock token comes from a read just before the write, so a concurrent
// change makes the call fail and the next run repeats it.
func (s *Sets) Update(ctx context.Context, ref collab.IPSetRef, addrs []string) error {
	scope, name, id, err := parts(ref.ARN)
	if err != nil {
		return err
	}
	cur, err := s.api.GetIPSet(ctx, &awswaf.GetIPSetInput{Scope: scope, Name: &name, Id: &id})
	if err != nil {
		return fmt.Errorf("wafv2: GetIPSet: %w", err)
	}
	if _, err := s.api.UpdateIPSet(ctx, &awswaf.UpdateIPSetInput{Scope: scope, Name: &name, Id: &id, Addresses: addrs, LockToken: cur.LockToken}); err != nil {
		return fmt.Errorf("wafv2: UpdateIPSet: %w", err)
	}
	return nil
}
