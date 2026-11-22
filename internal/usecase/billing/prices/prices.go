// Package prices is afs-billing's price sync (BI-3): when pricing.price_list_sync is on it reads the AWS
// Price List and proposes, per model whose on-demand price changed, one pending price proposal for a
// service admin to approve. An older pending proposal of the model is superseded first, an unchanged
// price proposes nothing, and every proposal publishes pricing.proposal_created in the transaction
// that stores it. Nothing here changes model_prices: approval does (M4-D2).
package prices

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Kind is what a list price is for.
type Kind string

// Price kinds.
const (
	Input      Kind = "input"
	Output     Kind = "output"
	CacheRead  Kind = "cache_read"
	CacheWrite Kind = "cache_write"
)

// ListPrice is one on-demand price of the Price List, normalized to USD per million tokens.
type ListPrice struct {
	// Product names the model as the list does: its model id when it has one, else its display name.
	Product       string
	Kind          Kind
	USDPerMtok    domain.Money
	EffectiveFrom time.Time
}

// PriceList reads the AWS Price List for Bedrock.
type PriceList interface {
	Fetch(ctx context.Context) ([]ListPrice, error)
}

// Set is the four prices of a model, USD per million tokens.
type Set struct {
	Input, Output, CacheRead, CacheWrite domain.Money
}

// Pending is a model's open proposal.
type Pending struct {
	ID            string
	EffectiveFrom time.Time
	Prices        Set
}

// ModelState is a catalog model with its latest price and open proposal.
type ModelState struct {
	ID string
	// Current is the model's latest price row, nil for a model without prices.
	Current *Set
	Pending *Pending
}

// Proposal is a new pending proposal.
type Proposal struct {
	ID            string
	ModelID       string
	EffectiveFrom time.Time
	Prices        Set
	FetchedAt     time.Time
	// Note says what changed.
	Note string
}

// Store is the billing store, used inside a transaction (except SyncEnabled).
type Store interface {
	// SyncEnabled reads pricing.price_list_sync. It runs outside the sync transaction, because the
	// billing role may be refused the setting (-db gap X-DB-9) and a refusal aborts a transaction.
	SyncEnabled(ctx context.Context) (bool, error)
	Models(ctx context.Context, tx port.Tx) ([]ModelState, error)
	// Supersede marks the pending proposal superseded.
	Supersede(ctx context.Context, tx port.Tx, proposalID string) error
	Insert(ctx context.Context, tx port.Tx, p Proposal) error
}

// Syncer runs the price sync.
type Syncer struct {
	List   PriceList
	Store  Store
	Tx     port.TxRunner
	Outbox port.OutboxWriter
	Clock  port.Clock
	IDs    port.IDGen
	Log    *slog.Logger
	// Session is the database session of the job (system:afs-billing).
	Session port.Session
}

// Result is what a sync did.
type Result struct {
	// Disabled is true when pricing.price_list_sync is off: the list was not even read.
	Disabled bool
	// Proposed counts new pending proposals; Superseded the older pending ones replaced or retired;
	// Unchanged the models whose list price equals the current price or the open proposal.
	Proposed, Superseded, Unchanged int
	// Unmatched lists list products that name no catalog model; Incomplete lists catalog models the list
	// prices only partly (input or output missing).
	Unmatched, Incomplete []string
}

// Run reads the list and proposes. The proposals of all models commit together with their events.
func (s *Syncer) Run(ctx context.Context) (Result, error) {
	var res Result
	on, err := s.Store.SyncEnabled(ctx)
	if err != nil {
		return res, fmt.Errorf("read pricing.price_list_sync: %w", err)
	}
	if !on {
		res.Disabled = true
		return res, nil
	}
	list, err := s.List.Fetch(ctx)
	if err != nil {
		return res, fmt.Errorf("fetch the price list: %w", err)
	}
	fetched := s.Clock.Now().UTC()
	err = s.Tx.InTx(ctx, s.Session, func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		models, err := s.Store.Models(ctx, tx)
		if err != nil {
			return err
		}
		targets, unmatched := match(list, models)
		res.Unmatched = unmatched
		for _, m := range models {
			t, ok := targets[m.ID]
			if !ok {
				continue
			}
			if !t.complete {
				res.Incomplete = append(res.Incomplete, m.ID)
				continue
			}
			want := t.set(m.Current)
			switch {
			case m.Current != nil && *m.Current == want:
				if m.Pending != nil { // the list went back to the current price: the open proposal is stale
					if err := s.Store.Supersede(ctx, tx, m.Pending.ID); err != nil {
						return err
					}
					res.Superseded++
				}
				res.Unchanged++
			case m.Pending != nil && m.Pending.Prices == want:
				res.Unchanged++
			default:
				if m.Pending != nil {
					if err := s.Store.Supersede(ctx, tx, m.Pending.ID); err != nil {
						return err
					}
					res.Superseded++
				}
				if err := s.propose(ctx, tx, m, want, t.effective, fetched); err != nil {
					return err
				}
				res.Proposed++
			}
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("propose prices: %w", err)
	}
	if s.Log != nil {
		s.Log.Info("price sync", "proposed", res.Proposed, "superseded", res.Superseded, "unchanged", res.Unchanged,
			"unmatched", len(res.Unmatched), "incomplete", res.Incomplete)
	}
	return res, nil
}

func (s *Syncer) propose(ctx context.Context, tx port.Tx, m ModelState, want Set, effective, fetched time.Time) error {
	pid, err := s.IDs.NewID("mpp")
	if err != nil {
		return err
	}
	if effective.IsZero() {
		effective = fetched
	}
	p := Proposal{ID: pid.String(), ModelID: m.ID, EffectiveFrom: effective, Prices: want, FetchedAt: fetched, Note: changeNote(m.Current, want)}
	if err := s.Store.Insert(ctx, tx, p); err != nil {
		return err
	}
	eid, err := s.IDs.NewID("evt")
	if err != nil {
		return err
	}
	data := map[string]any{
		"proposalId": p.ID, "modelId": m.ID, "effectiveFrom": effective.UTC().Format(time.RFC3339),
		"inputUsdPerMtok": want.Input.String(), "outputUsdPerMtok": want.Output.String(),
		"cacheReadUsdPerMtok": want.CacheRead.String(), "cacheWriteUsdPerMtok": want.CacheWrite.String(),
	}
	if m.Current != nil {
		data["currentInputUsdPerMtok"] = m.Current.Input.String()
		data["currentOutputUsdPerMtok"] = m.Current.Output.String()
	}
	ev, err := domain.NewEvent(domain.EventPricingProposalCreated, eid.String(), s.Clock.Now(), s.Session.Actor, "",
		"model_price_proposal", p.ID, data)
	if err != nil {
		return err
	}
	return s.Outbox.Publish(ctx, tx, ev)
}

func changeNote(cur *Set, want Set) string {
	if cur == nil {
		return "first price from the AWS Price List"
	}
	return fmt.Sprintf("AWS Price List: input %s -> %s, output %s -> %s per Mtok", cur.Input, want.Input, cur.Output, want.Output)
}

// target is what the list says of one model.
type target struct {
	in, out, cr, cw *domain.Money
	effective       time.Time
	complete        bool
}

// set returns the proposed prices: the list's figures, and for a cache price the list lacks the
// current one (zero without a current price), so a partial list never zeroes a price.
func (t target) set(cur *Set) Set {
	s := Set{Input: *t.in, Output: *t.out}
	if cur != nil {
		s.CacheRead, s.CacheWrite = cur.CacheRead, cur.CacheWrite
	}
	if t.cr != nil {
		s.CacheRead = *t.cr
	}
	if t.cw != nil {
		s.CacheWrite = *t.cw
	}
	return s
}

var (
	versionSuffix = regexp.MustCompile(`-v[0-9]+(:[0-9]+)?$`)
	nonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
)

// matchKey normalizes a model id ("anthropic.claude-sonnet-4-5-v1:0") or a list display name
// ("Claude Sonnet 4.5") to one comparable form ("claudesonnet45"): the provider prefix, the version
// suffix and every non-alphanumeric character are dropped.
func matchKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = versionSuffix.ReplaceAllString(s, "")
	if i := strings.Index(s, "."); i >= 0 && !strings.ContainsAny(s[:i], " ") {
		s = s[i+1:]
	}
	return nonAlnum.ReplaceAllString(s, "")
}

// match groups list prices by catalog model: a product matches a model by its exact id, else by the
// normalized key. It returns the targets by model id and the products that match no model.
func match(list []ListPrice, models []ModelState) (map[string]target, []string) {
	byID := map[string]string{}
	byKey := map[string]string{}
	for _, m := range models {
		byID[m.ID] = m.ID
		byKey[matchKey(m.ID)] = m.ID
	}
	out := map[string]target{}
	seenUnmatched := map[string]bool{}
	var unmatched []string
	for _, lp := range list {
		id, ok := byID[lp.Product]
		if !ok {
			id, ok = byKey[matchKey(lp.Product)]
		}
		if !ok {
			if !seenUnmatched[lp.Product] {
				seenUnmatched[lp.Product] = true
				unmatched = append(unmatched, lp.Product)
			}
			continue
		}
		t := out[id]
		p := lp.USDPerMtok
		switch lp.Kind {
		case Input:
			t.in = &p
		case Output:
			t.out = &p
		case CacheRead:
			t.cr = &p
		case CacheWrite:
			t.cw = &p
		default:
			continue
		}
		if lp.EffectiveFrom.After(t.effective) {
			t.effective = lp.EffectiveFrom
		}
		out[id] = t
	}
	for id, t := range out {
		t.complete = t.in != nil && t.out != nil
		out[id] = t
	}
	return out, unmatched
}
