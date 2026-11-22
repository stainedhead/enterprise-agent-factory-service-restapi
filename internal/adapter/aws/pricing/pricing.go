// Package pricing reads the AWS Price List for Amazon Bedrock (BI-3): the on-demand token prices of
// one region, normalized to USD per million tokens, for afs-billing's price sync.
//
// It calls GetProducts (ServiceCode AmazonBedrock, filter regionCode) and parses each product's JSON
// document: attributes.model (or attributes.modelId) names the model, attributes.inferenceType says
// input, output, cache read or cache write, and terms.OnDemand carries the price dimension (unit
// "1K tokens", "1M tokens" or "tokens", pricePerUnit.USD) and the term's effectiveDate. Batch,
// provisioned-throughput and custom-model products are not on-demand token prices and are left out.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
)

// ServiceCode is the Price List service code of Bedrock.
const ServiceCode = "AmazonBedrock"

// API is the part of the Pricing client the adapter uses.
type API interface {
	GetProducts(ctx context.Context, in *awspricing.GetProductsInput, opts ...func(*awspricing.Options)) (*awspricing.GetProductsOutput, error)
}

// Config selects the prices.
type Config struct {
	// Region is the AWS region whose prices are read (regionCode); default us-east-1.
	Region string
	// MaxPages bounds the pagination (default 200).
	MaxPages int
}

// List implements prices.PriceList on the Price List API.
type List struct {
	api API
	cfg Config
	log *slog.Logger
}

var _ prices.PriceList = (*List)(nil)

// New returns a List on the SDK configuration. The Price List API is served from a few regions only
// (us-east-1, ap-south-1, eu-central-1), so awsCfg must name one of them.
func New(awsCfg awssdk.Config, c Config, log *slog.Logger) *List {
	return newList(awspricing.NewFromConfig(awsCfg), c, log)
}

func newList(api API, c Config, log *slog.Logger) *List {
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.MaxPages <= 0 {
		c.MaxPages = 200
	}
	return &List{api: api, cfg: c, log: log}
}

// Fetch implements prices.PriceList. The result is in a stable order.
func (l *List) Fetch(ctx context.Context) ([]prices.ListPrice, error) {
	var out []prices.ListPrice
	var token *string
	for page := 0; ; page++ {
		if page >= l.cfg.MaxPages {
			return nil, fmt.Errorf("pricing: more than %d pages of products", l.cfg.MaxPages)
		}
		res, err := l.api.GetProducts(ctx, &awspricing.GetProductsInput{
			ServiceCode: awssdk.String(ServiceCode), NextToken: token, FormatVersion: awssdk.String("aws_v1"),
			Filters: []types.Filter{{Type: types.FilterTypeTermMatch, Field: awssdk.String("regionCode"), Value: awssdk.String(l.cfg.Region)}},
		})
		if err != nil {
			return nil, fmt.Errorf("pricing: GetProducts: %w", err)
		}
		for _, doc := range res.PriceList {
			ps, err := parseProduct(doc, l.cfg.Region)
			if err != nil {
				if l.log != nil {
					l.log.Warn("price list product skipped", "err", err)
				}
				continue
			}
			out = append(out, ps...)
		}
		if res.NextToken == nil || *res.NextToken == "" {
			break
		}
		token = res.NextToken
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Product != out[j].Product {
			return out[i].Product < out[j].Product
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

type productDoc struct {
	Product struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			EffectiveDate   string `json:"effectiveDate"`
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

var errNotTokenPrice = errors.New("not an on-demand token price")

// parseProduct returns the prices of one product document: none for a product that is not an
// on-demand token price of the region, an error for a malformed one.
func parseProduct(doc, region string) ([]prices.ListPrice, error) {
	var d productDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, fmt.Errorf("unreadable product: %w", err)
	}
	a := d.Product.Attributes
	if rc := a["regionCode"]; rc != "" && rc != region {
		return nil, nil
	}
	kind, ok := classify(a)
	if !ok {
		return nil, nil
	}
	name := a["modelId"]
	if name == "" {
		name = a["model"]
	}
	if name == "" {
		return nil, fmt.Errorf("product %s has no model", a["usagetype"])
	}
	var out []prices.ListPrice
	for _, term := range d.Terms.OnDemand {
		eff, _ := time.Parse(time.RFC3339, term.EffectiveDate)
		for _, dim := range term.PriceDimensions {
			usd, err := perMtok(dim.Unit, dim.PricePerUnit["USD"])
			if err != nil {
				return nil, fmt.Errorf("product %s: %w", a["usagetype"], err)
			}
			out = append(out, prices.ListPrice{Product: name, Kind: kind, USDPerMtok: usd, EffectiveFrom: eff.UTC()})
		}
	}
	return out, nil
}

// classify maps a product's attributes to a price kind; false for anything that is not an on-demand
// token price.
func classify(a map[string]string) (prices.Kind, bool) {
	all := strings.ToLower(a["feature"] + " " + a["usagetype"] + " " + a["inferenceType"])
	for _, skip := range []string{"batch", "provisioned", "custom"} {
		if strings.Contains(all, skip) {
			return "", false
		}
	}
	t := strings.ToLower(a["inferenceType"])
	switch {
	case strings.Contains(t, "cache") && strings.Contains(t, "read"):
		return prices.CacheRead, true
	case strings.Contains(t, "cache") && strings.Contains(t, "write"):
		return prices.CacheWrite, true
	case strings.Contains(t, "input") && strings.Contains(t, "token"):
		return prices.Input, true
	case strings.Contains(t, "output") && strings.Contains(t, "token"):
		return prices.Output, true
	}
	return "", false
}

// perMtok converts a price per unit to USD per million tokens, rounded to the database's six decimals.
func perMtok(unit, usd string) (domain.Money, error) {
	var factor int64
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "1k tokens", "1000 tokens":
		factor = 1_000
	case "1m tokens", "1000000 tokens":
		factor = 1
	case "tokens", "token", "1 token":
		factor = 1_000_000
	default:
		return domain.Money{}, fmt.Errorf("%w: unit %q", errNotTokenPrice, unit)
	}
	r, ok := new(big.Rat).SetString(usd)
	if !ok || r.Sign() < 0 {
		return domain.Money{}, fmt.Errorf("price %q is not a decimal", usd)
	}
	r.Mul(r, big.NewRat(factor, 1))
	return domain.ParseMoney("price", r.FloatString(6))
}
