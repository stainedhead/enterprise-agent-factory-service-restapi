package billingpull

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// ErrDayNotAvailable is returned for a day a provider does not report.
var ErrDayNotAvailable = errors.New("billingpull: the provider does not report that day")

// OpenRouter pulls the usage of each API key (GET /api/v1/keys, a provisioning key) and names each key's team
// from the key name. OpenRouter reports usage_daily for the current UTC day only, so only today can be pulled;
// afs-billing pulls it late in the day.
type OpenRouter struct {
	// BaseURL defaults to https://openrouter.ai.
	BaseURL string
	Client  Doer
	Key     Credential
	// Now is the clock (UTC day check).
	Now func() time.Time
}

// Provider implements routebill.Puller.
func (o *OpenRouter) Provider() string { return "openrouter" }

type openRouterKeys struct {
	Data []struct {
		Name       string  `json:"name"`
		Label      string  `json:"label"`
		UsageDaily float64 `json:"usage_daily"`
	} `json:"data"`
}

// Pull implements routebill.Puller.
func (o *OpenRouter) Pull(ctx context.Context, ep routebill.Endpoint, day time.Time) ([]routebill.Figure, domain.Money, error) {
	if today := o.Now().UTC().Truncate(24 * time.Hour); !day.UTC().Truncate(24 * time.Hour).Equal(today) {
		return nil, domain.Money{}, fmt.Errorf("%w: openrouter reports usage_daily for %s only", ErrDayNotAvailable, today.Format(time.DateOnly))
	}
	key, err := o.Key(ctx, ep)
	if err != nil {
		return nil, domain.Money{}, err
	}
	base := o.BaseURL
	if base == "" {
		base = "https://openrouter.ai"
	}
	var a attribution
	for offset := 0; ; offset += 100 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/keys?offset=%d", strings.TrimRight(base, "/"), offset), nil)
		if err != nil {
			return nil, domain.Money{}, err
		}
		var body openRouterKeys
		if err := call(o.Client, req, key, &body); err != nil {
			return nil, domain.Money{}, err
		}
		for _, k := range body.Data {
			team, ok := TeamOf(k.Name)
			if !ok {
				team, ok = TeamOf(k.Label)
			}
			a.add(team, ok, usd(k.UsageDaily))
		}
		if len(body.Data) < 100 {
			break
		}
	}
	figs, un := a.result()
	return figs, un, nil
}
