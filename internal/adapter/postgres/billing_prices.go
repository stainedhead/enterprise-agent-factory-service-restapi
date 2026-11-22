package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// PriceStore implements prices.Store for afs-billing's login: it reads models and their prices and
// writes price proposals (database contract §3: afs_billing selects model_prices and inserts and
// supersedes model_price_proposals).
type PriceStore struct {
	runner *TxRunner
	// fallback answers pricing.price_list_sync while the billing role cannot read service_settings
	// (-db gap X-DB-9); nil fails the sync closed instead.
	fallback *bool
}

// NewPriceStore returns a PriceStore on the billing pool's runner. fallback is the configured value of
// pricing.price_list_sync used only when the database refuses the setting to the billing role.
func NewPriceStore(runner *TxRunner, fallback *bool) *PriceStore {
	return &PriceStore{runner: runner, fallback: fallback}
}

var _ prices.Store = (*PriceStore)(nil)

// ErrSyncSettingUnavailable is returned when the billing role cannot read pricing.price_list_sync and
// no fallback is configured: the sync does nothing rather than guess whether a service admin switched it off.
var ErrSyncSettingUnavailable = errors.New("pricing.price_list_sync is not readable by the billing role (db gap X-DB-9) and AFS_PRICING_PRICE_LIST_SYNC is not set")

// SyncEnabled implements prices.Store. An unset setting row means the default, on.
func (s *PriceStore) SyncEnabled(ctx context.Context) (bool, error) {
	on := true
	err := s.runner.ReadTx(ctx, BillingSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var raw []byte
		err = t.QueryRow(ctx, `SELECT value FROM afs.service_settings WHERE key = 'pricing.price_list_sync'`).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &on)
	})
	if err != nil {
		if e := MapError(err, OpRead, "pricing.price_list_sync"); domain.KindOf(e) == domain.KindAccessDenied {
			if s.fallback == nil {
				return false, ErrSyncSettingUnavailable
			}
			return *s.fallback, nil
		}
		return false, MapError(err, OpRead, "pricing.price_list_sync")
	}
	return on, nil
}

// Models implements prices.Store.
func (s *PriceStore) Models(ctx context.Context, tx port.Tx) ([]prices.ModelState, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `
SELECT m.id,
       c.input_usd_per_mtok::text, c.output_usd_per_mtok::text, c.cache_read_usd_per_mtok::text, c.cache_write_usd_per_mtok::text,
       p.id, p.effective_from, p.input_usd_per_mtok::text, p.output_usd_per_mtok::text,
       p.cache_read_usd_per_mtok::text, p.cache_write_usd_per_mtok::text
FROM afs.models m
LEFT JOIN LATERAL (SELECT * FROM afs.model_prices WHERE model_id = m.id ORDER BY effective_from DESC LIMIT 1) c ON true
LEFT JOIN afs.model_price_proposals p ON p.model_id = m.id AND p.status = 'pending'
ORDER BY m.id`)
	if err != nil {
		return nil, MapError(err, OpRead, "models")
	}
	defer rows.Close()
	out := []prices.ModelState{}
	for rows.Next() {
		var ms prices.ModelState
		var cur [4]*string
		var pid *string
		var peff *time.Time
		var pend [4]*string
		if err := rows.Scan(&ms.ID, &cur[0], &cur[1], &cur[2], &cur[3], &pid, &peff, &pend[0], &pend[1], &pend[2], &pend[3]); err != nil {
			return nil, MapError(err, OpRead, "models")
		}
		if cur[0] != nil {
			set, err := priceSet(cur)
			if err != nil {
				return nil, err
			}
			ms.Current = &set
		}
		if pid != nil {
			set, err := priceSet(pend)
			if err != nil {
				return nil, err
			}
			ms.Pending = &prices.Pending{ID: *pid, EffectiveFrom: *peff, Prices: set}
		}
		out = append(out, ms)
	}
	return out, MapError(rows.Err(), OpRead, "models")
}

func priceSet(v [4]*string) (prices.Set, error) {
	var s prices.Set
	for i, dst := range []*domain.Money{&s.Input, &s.Output, &s.CacheRead, &s.CacheWrite} {
		if v[i] == nil {
			continue
		}
		m, err := domain.ParseMoney("price", *v[i])
		if err != nil {
			return s, err
		}
		*dst = m
	}
	return s, nil
}

// Supersede implements prices.Store. Only the billing role may move a proposal to superseded.
func (s *PriceStore) Supersede(ctx context.Context, tx port.Tx, proposalID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `UPDATE afs.model_price_proposals SET status = 'superseded' WHERE id = $1 AND status = 'pending'`, proposalID)
	return MapError(err, OpWrite, "price proposal")
}

// Insert implements prices.Store.
func (s *PriceStore) Insert(ctx context.Context, tx port.Tx, p prices.Proposal) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.model_price_proposals
(id, model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, cache_read_usd_per_mtok, cache_write_usd_per_mtok, fetched_at, note)
VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8, $9)`,
		p.ID, p.ModelID, p.EffectiveFrom, p.Prices.Input.String(), p.Prices.Output.String(),
		p.Prices.CacheRead.String(), p.Prices.CacheWrite.String(), p.FetchedAt, p.Note)
	return MapError(err, OpWrite, "price proposal")
}
