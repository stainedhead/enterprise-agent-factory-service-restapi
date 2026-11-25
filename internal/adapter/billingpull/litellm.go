package billingpull

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// LiteLLMPricesURL is LiteLLM's published model price list (MIT licensed data, MEG-D2).
const LiteLLMPricesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// LiteLLM reads LiteLLM's price list into route list prices (USD per million tokens).
type LiteLLM struct {
	// URL defaults to LiteLLMPricesURL.
	URL    string
	Client Doer
}

// litellmProviders maps a litellm_provider to the AFS inference provider.
func litellmProvider(p string) string {
	switch {
	case p == "openai" || p == "text-completion-openai":
		return "openai"
	case p == "openrouter":
		return "openrouter"
	case p == "azure" || p == "azure_ai":
		return "azure"
	case strings.HasPrefix(p, "vertex_ai"):
		return "google"
	}
	return ""
}

type litellmEntry struct {
	Input    *float64 `json:"input_cost_per_token"`
	Output   *float64 `json:"output_cost_per_token"`
	Provider string   `json:"litellm_provider"`
}

// Fetch returns the list's per-token prices of the providers AFS routes reach. The model name is the list key
// without its provider prefix (openrouter/x/y is x/y), as a route lists it.
func (l *LiteLLM) Fetch(ctx context.Context) ([]routebill.ListedPrice, error) {
	u := l.URL
	if u == "" {
		u = LiteLLMPricesURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := call(l.Client, req, "", &raw); err != nil {
		return nil, err
	}
	var out []routebill.ListedPrice
	for name, msg := range raw {
		var e litellmEntry
		if json.Unmarshal(msg, &e) != nil || e.Input == nil || e.Output == nil {
			continue // sample_spec and entries without token prices
		}
		prov := litellmProvider(e.Provider)
		if prov == "" || *e.Input < 0 || *e.Output < 0 {
			continue
		}
		model := name
		if i := strings.Index(name, "/"); i > 0 && litellmProvider(name[:i]) == prov {
			model = name[i+1:]
		}
		out = append(out, routebill.ListedPrice{Provider: prov, Model: model, Input: usd(*e.Input * 1e6), Output: usd(*e.Output * 1e6)})
	}
	slices.SortFunc(out, func(a, b routebill.ListedPrice) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out, nil
}
