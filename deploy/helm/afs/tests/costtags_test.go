package tests

import "testing"

// M5-F1: the billing chart carries the daily cost allocation tag check.
func TestBillingCostTagsJob(t *testing.T) {
	docs := render(t, "")
	cj := find(t, docs, "CronJob", "afs-billing-cost-tags")
	if got := str(path(cj, "spec"), "schedule"); got != "30 6 * * *" {
		t.Errorf("schedule = %q", got)
	}
}
