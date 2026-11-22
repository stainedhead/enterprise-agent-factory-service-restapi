package matrix

import (
	"testing"
	"time"
)

// Case of M5-F2 (ListCostTagStatus, admin:cost-tag-status): a read for service admins only; every other
// caller is refused with 403 and one denied audit record. The row is seeded before every call so a
// service admin always meets a key.
func init() {
	Register(Case{Op: "ListCostTagStatus", Path: "/v1/admin/cost-tag-status",
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `INSERT INTO afs.cost_allocation_tag_status (key, seen_in_cur, first_seen_at, last_seen_at, aws_status, checked_at)
VALUES ('mx-cost-tag', true, $1, $1, 'active', now()) ON CONFLICT (key) DO NOTHING`, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
			return nil
		}})
}
