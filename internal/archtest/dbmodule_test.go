package archtest_test

import (
	"fmt"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-db/migrate"
	"github.com/stainedhead/enterprise-agent-factory-service-db/migrations"
)

// SchemaVersion is the -db schema version this repository is built against (plan.md §6).
const schemaVersion = 19

// TestDBModuleMigrations: the adopted -db module embeds migrations 000001 to 000019.
func TestDBModuleMigrations(t *testing.T) {
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != schemaVersion {
		t.Fatalf("migrations = %d, want %d", len(migs), schemaVersion)
	}
	for i, m := range migs {
		if m.Version != i+1 {
			t.Errorf("migration %d has version %06d", i, m.Version)
		}
	}
	if got := fmt.Sprintf("%06d", migs[len(migs)-1].Version); got != "000019" {
		t.Errorf("latest = %s", got)
	}
}
