package main

import "testing"

func TestCostTagsJobIsRegistered(t *testing.T) {
	if _, ok := jobs["cost-tags"]; !ok {
		t.Fatal("cost-tags is not registered (deploy/helm/afs/values/billing.yaml)")
	}
}
