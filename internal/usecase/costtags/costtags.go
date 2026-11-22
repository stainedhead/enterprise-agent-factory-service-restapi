// Package costtags implements ListCostTagStatus (API contract §4.9, M5-F2, RA-11, TAG-7): for each AFS
// tag key, whether the Cost and Usage Report has shown it and whether it is active as a cost allocation
// tag in AWS Billing. Only service admins read it: the S9 banner that asks them to activate the tags
// is built on it. The rows are written by afs-billing's daily check (usecase/billing/costtags); the
// banner is acknowledged with PutSetting on billing.tag_activation_acknowledged, which the settings use
// case validates (a boolean) and audits.
package costtags

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// AWS activation statuses as the API reports them.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
	StatusUnknown  = "unknown"
)

// Status is one tag key's status.
type Status struct {
	Key         string
	SeenInCUR   bool
	FirstSeenAt *time.Time
	LastSeenAt  *time.Time
	// AWSStatus is active, inactive or unknown (AWS lists no such tag).
	AWSStatus string
	CheckedAt time.Time
}

// Repository reads the status table.
type Repository interface {
	// List returns statuses with key greater than after, in key order, at most limit.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]Status, error)
}

// Service is the cost tag status use case.
type Service struct {
	acting.Deps
	Repo Repository
}

// List implements ListCostTagStatus (admin:cost-tag-status, service scope): a team-admin or any other
// caller gets 403 and one denied audit record.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]Status, string, error) {
	if !authz.Authorize(c.Principal, authz.AdminCostTagStatus, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil, "", s.Denied(ctx, c, "ListCostTagStatus", "", authz.AdminCostTagStatus)
	}
	p = p.Normalized()
	var rows []Status
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r Status) string { return r.Key })
	return page, next, nil
}
