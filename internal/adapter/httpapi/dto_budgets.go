package httpapi

import (
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// budgetBody is the Budget of API contract §5: the stored budget and its current-period position.
// expiresAt and projectedCapAt are null when absent.
type budgetBody struct {
	ID             string              `json:"id"`
	Period         domain.BudgetPeriod `json:"period"`
	AmountUsd      domain.Money        `json:"amountUsd"`
	Thresholds     []int               `json:"thresholds"`
	OnExhaust      domain.OnExhaust    `json:"onExhaust"`
	ExpiresAt      *string             `json:"expiresAt"`
	SpentUsd       domain.Money        `json:"spentUsd"`
	ReservedUsd    domain.Money        `json:"reservedUsd"`
	PercentUsed    float64             `json:"percentUsed"`
	ForecastUsd    domain.Money        `json:"forecastUsd"`
	ProjectedCapAt *string             `json:"projectedCapAt"`
	Version        domain.Version      `json:"version"`
	CreatedAt      string              `json:"createdAt"`
	CreatedBy      string              `json:"createdBy"`
	UpdatedAt      string              `json:"updatedAt"`
}

func toBudgetBody(p domain.BudgetPosition) budgetBody {
	return budgetBody{ID: p.ID, Period: p.Period, AmountUsd: p.Amount, Thresholds: p.Thresholds, OnExhaust: p.OnExhaust,
		ExpiresAt: w2TimePtr(p.ExpiresAt), SpentUsd: p.Spent, ReservedUsd: p.Reserved, PercentUsed: p.PercentUsed,
		ForecastUsd: p.Forecast, ProjectedCapAt: w2TimePtr(p.ProjectedCapAt), Version: p.Version,
		CreatedAt: w2Time(p.CreatedAt), CreatedBy: p.CreatedBy, UpdatedAt: w2Time(p.UpdatedAt)}
}

// putBudgetRequest is the PutBudgetRequest of API contract §5. period and amountUsd are required; a
// member left out takes its default (PUT replaces the budget).
type putBudgetRequest struct {
	Period     *domain.BudgetPeriod `json:"period"`
	AmountUsd  *domain.Money        `json:"amountUsd"`
	Thresholds []int                `json:"thresholds"`
	OnExhaust  *domain.OnExhaust    `json:"onExhaust"`
	ExpiresAt  *time.Time           `json:"expiresAt"`
}

// params validates the required members and returns the domain request.
func (q putBudgetRequest) params() (domain.PutBudgetParams, error) {
	var fe []domain.FieldError
	if q.Period == nil {
		fe = append(fe, domain.FieldError{Field: "period", Reason: "is required"})
	}
	if q.AmountUsd == nil {
		fe = append(fe, domain.FieldError{Field: "amountUsd", Reason: "is required"})
	}
	if len(fe) > 0 {
		return domain.PutBudgetParams{}, domain.Validation(fe...)
	}
	p := domain.PutBudgetParams{Period: *q.Period, Amount: *q.AmountUsd, Thresholds: q.Thresholds, ExpiresAt: q.ExpiresAt}
	if q.OnExhaust != nil {
		p.OnExhaust = *q.OnExhaust
	}
	return p, nil
}
