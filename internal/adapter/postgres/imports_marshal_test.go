package postgres

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type noIDs struct{}

func (noIDs) NewID(p domain.IDPrefix) (domain.ID, error) { return domain.ID(string(p) + "_x"), nil }

// FR-R17: a value that cannot be marshalled fails the import step and writes nothing, on both the
// insert and the update path.
func TestImportUpsertMarshalErrorFailsAndWritesNothing(t *testing.T) {
	db := pgtest.Get(t)
	cases := []struct {
		name string
		find string
	}{
		{"insert", `SELECT id FROM afs.budgets WHERE false`},
		{"update", `SELECT 'bud_existing'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := db.Admin(t)
			tx, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			var before int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM afs.budgets`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			im := &importer{t: &Tx{Tx: tx}, ids: noIDs{}}
			tb := tbl{name: "budgets", prefix: domain.PrefixBudget, cols: []string{"amount_usd"}, upd: []string{"amount_usd"}}
			_, _, err = im.upsert(t.Context(), tb, tc.find, nil, domain.Row{"amount_usd": make(chan int)}, nil, "bad")
			if err == nil {
				t.Fatal("expected a marshal error")
			}
			var ue *json.UnsupportedTypeError
			if !errors.As(err, &ue) {
				t.Fatalf("error does not wrap the marshal error: %v", err)
			}
			if !strings.Contains(err.Error(), "budgets") {
				t.Fatalf("error does not name the table: %v", err)
			}
			if len(im.changes) != 0 {
				t.Fatalf("changes recorded: %v", im.changes)
			}
			var after int
			if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM afs.budgets`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("rows changed %d -> %d", before, after)
			}
		})
	}
}
