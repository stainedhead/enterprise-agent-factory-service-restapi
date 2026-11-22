package gateway

import (
	"math/big"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// DefaultMaxOutputTokens is the worst-case output of a model whose catalog row has no
// max_output_tokens (a legacy row; the API requires the field, D28).
const DefaultMaxOutputTokens = 8192

const microsPerMtok = 1_000_000

// OutputCap returns the output tokens the call may produce: the request's maxTokens, capped to the
// model's max_output_tokens, else the model's max (gateway contract §4, CT-GW-4).
func OutputCap(requested, modelMax int) int {
	if modelMax <= 0 {
		modelMax = DefaultMaxOutputTokens
		if requested > 0 {
			return requested
		}
	}
	if requested > 0 && requested < modelMax {
		return requested
	}
	return modelMax
}

// InputEstimate is the input tokens estimated from the request body: ceil(bytes / 3).
func InputEstimate(bodyBytes int) int {
	if bodyBytes <= 0 {
		return 0
	}
	return (bodyBytes + 2) / 3
}

// tokensCost returns tokens x price per million tokens in micro-dollars, exactly.
func tokensCost(tokens int, perMtok domain.Money) *big.Int {
	c := new(big.Int).Mul(big.NewInt(int64(tokens)), big.NewInt(perMtok.Micros()))
	return c
}

// ceilMicros divides a sum of token costs by one million, rounding up, and returns micro-dollars.
func ceilMicros(sum *big.Int) domain.Money {
	q, r := new(big.Int).QuoRem(sum, big.NewInt(microsPerMtok), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return domain.MoneyFromMicros(q.Int64())
}

// ReserveAmount is the worst-case cost reserved before the call:
//
//	output_cap x output price + ceil(bytes / 3) x input price
//
// with the price effective at the time of the call. It is at least one micro-dollar, because a
// reservation of zero is refused by the database.
func ReserveAmount(p Price, outputCap, bodyBytes int) domain.Money {
	sum := new(big.Int).Add(tokensCost(outputCap, p.Output), tokensCost(InputEstimate(bodyBytes), p.Input))
	m := ceilMicros(sum)
	if m.Micros() < 1 {
		return domain.MoneyFromMicros(1)
	}
	return m
}

// ActualCost is the settled cost of a response's usage: input, output, cache read and cache write
// tokens at the price effective at the call.
func ActualCost(p Price, u Usage) domain.Money {
	sum := new(big.Int)
	sum.Add(sum, tokensCost(u.Input, p.Input))
	sum.Add(sum, tokensCost(u.Output, p.Output))
	sum.Add(sum, tokensCost(u.CacheRead, p.CacheRead))
	sum.Add(sum, tokensCost(u.CacheWrite, p.CacheWrite))
	return ceilMicros(sum)
}

// EffectivePrice picks the price in effect at t from rows newest first.
func EffectivePrice(rows []Price, t time.Time) (Price, bool) {
	for _, r := range rows {
		if !r.EffectiveFrom.After(t) {
			return r, true
		}
	}
	return Price{}, false
}
