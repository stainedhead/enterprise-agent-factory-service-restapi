package gateway

import "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"

// SettleAmount is the actual cost added to the period when a call ends (gateway contract §4, §5):
//
//   - the call finished and usage was reported: the cost of that usage;
//   - the upstream never started to answer (an error before the first event): zero, so the whole
//     reservation is released;
//   - it started and failed or was aborted, usage reported: the tokens seen so far, and the rest of
//     the reservation is released;
//   - it started and no usage was reported: the reservation itself, the worst case stays charged.
func SettleAmount(res Result, callErr error, p Price, reserved domain.Money) domain.Money {
	switch {
	case callErr == nil && res.UsageReported:
		return ActualCost(p, res.Usage)
	case callErr == nil:
		return reserved
	case !res.Started:
		return domain.MoneyFromMicros(0)
	case res.UsageReported:
		return ActualCost(p, res.Usage)
	}
	return reserved
}
