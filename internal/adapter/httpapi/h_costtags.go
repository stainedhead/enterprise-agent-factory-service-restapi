package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costtags"
)

const costTagsAreaName = "costtags"

func init() { Areas = append(Areas, Area{costTagsAreaName, none}) }

// CostTagsAPI serves ListCostTagStatus (M5-F2, RA-11).
type CostTagsAPI struct {
	Svc   *costtags.Service
	Pages PageTokens
}

// BindCostTags binds the handler as the costtags area.
func BindCostTags(a *CostTagsAPI) { w2SetArea(costTagsAreaName, a.Register) }

// Register binds the operation.
func (a *CostTagsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"ListCostTagStatus": a.list})
}

// costTagStatusBody is the CostTagStatus schema: the timestamps are null until the CUR has shown the key.
type costTagStatusBody struct {
	Key         string  `json:"key"`
	SeenInCur   bool    `json:"seenInCur"`
	FirstSeenAt *string `json:"firstSeenAt"`
	LastSeenAt  *string `json:"lastSeenAt"`
	AwsStatus   string  `json:"awsStatus"`
	CheckedAt   string  `json:"checkedAt"`
}

func toCostTagStatusBody(s costtags.Status) costTagStatusBody {
	return costTagStatusBody{Key: s.Key, SeenInCur: s.SeenInCUR, FirstSeenAt: w2TimePtr(s.FirstSeenAt), LastSeenAt: w2TimePtr(s.LastSeenAt),
		AwsStatus: s.AWSStatus, CheckedAt: w2Time(s.CheckedAt)}
}

func (a *CostTagsAPI) list(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]costtags.Status, string, error) {
		return a.Svc.List(r.Context(), c, p)
	}, toCostTagStatusBody)
}
