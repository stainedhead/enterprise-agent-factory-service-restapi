package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// operation is a Route prepared for net/http: Go's ServeMux cannot match a wildcard followed by
// ":verb" in one segment, so the verb is stripped, the base pattern registered once, and requests
// dispatched by verb.
type operation struct {
	Route
	base      string // path without the :verb suffix
	verb      string
	wildcard  string // name of the last segment's wildcard, if any
	query     map[string]bool
	bodyLimit int64
	handler   http.Handler
}

func splitVerb(path string) (base, verb, wildcard string) {
	i := strings.LastIndex(path, "/")
	last := path[i+1:]
	if j := strings.Index(last, "}:"); j >= 0 {
		verb = last[j+2:]
		last = last[:j+1]
	} else if j := strings.Index(last, ":"); j >= 0 && !strings.HasPrefix(last, "{") {
		// A literal segment with a verb: ServeMux matches it as a literal.
		return path, "", ""
	}
	if strings.HasPrefix(last, "{") && strings.HasSuffix(last, "}") {
		wildcard = strings.TrimSuffix(strings.TrimPrefix(last, "{"), "}")
	}
	return path[:i+1] + last, verb, wildcard
}

var (
	queryOnce   sync.Once
	queryParams map[string]map[string]bool
	queryErr    error
)

// operationQueryParams reads the query parameters of every operation from the embedded OpenAPI
// document, so strict query checking follows the contract exactly.
func operationQueryParams() (map[string]map[string]bool, error) {
	queryOnce.Do(func() {
		var doc struct {
			Paths      map[string]map[string]yaml.Node `yaml:"paths"`
			Components struct {
				Parameters map[string]struct {
					In   string `yaml:"in"`
					Name string `yaml:"name"`
				} `yaml:"parameters"`
			} `yaml:"components"`
		}
		if queryErr = yaml.Unmarshal(api.OpenAPI, &doc); queryErr != nil {
			return
		}
		queryParams = map[string]map[string]bool{}
		for _, item := range doc.Paths {
			for method, node := range item {
				if method == "parameters" {
					continue
				}
				var op struct {
					OperationID string `yaml:"operationId"`
					Parameters  []struct {
						Ref  string `yaml:"$ref"`
						In   string `yaml:"in"`
						Name string `yaml:"name"`
					} `yaml:"parameters"`
				}
				if err := node.Decode(&op); err != nil {
					queryErr = err
					return
				}
				set := map[string]bool{}
				for _, p := range op.Parameters {
					in, name := p.In, p.Name
					if p.Ref != "" {
						c := doc.Components.Parameters[strings.TrimPrefix(p.Ref, "#/components/parameters/")]
						in, name = c.In, c.Name
					}
					if in == "query" {
						set[name] = true
					}
				}
				queryParams[op.OperationID] = set
			}
		}
	})
	return queryParams, queryErr
}

func buildOperations() (map[string]*operation, error) {
	qp, err := operationQueryParams()
	if err != nil {
		return nil, fmt.Errorf("httpapi: read OpenAPI parameters: %w", err)
	}
	ops := make(map[string]*operation, len(Routes))
	for _, rt := range Routes {
		if _, dup := ops[rt.OperationID]; dup {
			return nil, fmt.Errorf("httpapi: duplicate operation %s", rt.OperationID)
		}
		q, ok := qp[rt.OperationID]
		if !ok {
			return nil, fmt.Errorf("httpapi: %s missing from api/openapi.yaml", rt.OperationID)
		}
		base, verb, wc := splitVerb(rt.Path)
		limit := int64(DefaultBodyLimit)
		if l, ok := bodyLimits[rt.OperationID]; ok {
			limit = l
		}
		ops[rt.OperationID] = &operation{Route: rt, base: base, verb: verb, wildcard: wc, query: q, bodyLimit: limit,
			handler: placeholder(rt.OperationID)}
	}
	return ops, nil
}

// placeholder answers 501 with InternalError and details.reason not_implemented until a stream
// binds the operation (N-6).
func placeholder(opID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := domain.Internal(nil)
		e.Message = opID + " is not implemented yet"
		e.Reason = domain.ReasonNotImplemented
		writeErrorStatus(w, r, e, http.StatusNotImplemented)
	})
}

// RouteFrom returns the route of the request being served.
func RouteFrom(r *http.Request) (Route, bool) {
	op, ok := r.Context().Value(keyRoute).(*operation)
	if !ok {
		return Route{}, false
	}
	return op.Route, true
}

// mount registers every operation on mux, grouping operations that share a method and base
// pattern behind a verb dispatcher.
func mount(mux *http.ServeMux, ops map[string]*operation, wrap func(*operation) http.Handler) {
	type group struct {
		wildcard string
		byVerb   map[string]http.Handler
	}
	groups := map[string]*group{}
	for _, op := range ops {
		key := op.Method + " " + op.base
		g := groups[key]
		if g == nil {
			g = &group{wildcard: op.wildcard, byVerb: map[string]http.Handler{}}
			groups[key] = g
		}
		g.byVerb[op.verb] = wrap(op)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := groups[key]
		if g.wildcard == "" {
			mux.Handle(key, g.byVerb[""])
			continue
		}
		mux.Handle(key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			val := r.PathValue(g.wildcard)
			verb := ""
			// Only a registered verb is split off, so ids that contain a colon (Bedrock model ids such as
			// "anthropic.claude-v2:0") stay whole.
			if i := strings.LastIndex(val, ":"); i >= 0 && verb == "" {
				if _, known := g.byVerb[val[i+1:]]; known && val[i+1:] != "" {
					val, verb = val[:i], val[i+1:]
				}
			}
			h, ok := g.byVerb[verb]
			if !ok || val == "" {
				WriteError(w, r, domain.NotFound("resource"))
				return
			}
			r.SetPathValue(g.wildcard, val)
			h.ServeHTTP(w, r)
		}))
	}
}

// checkQuery refuses query parameters the operation does not declare.
func checkQuery(op *operation, r *http.Request) error {
	var unknown []domain.FieldError
	for name := range r.URL.Query() {
		if !op.query[name] {
			unknown = append(unknown, domain.FieldError{Field: name, Reason: "unknown query parameter"})
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.SortFunc(unknown, func(a, b domain.FieldError) int { return strings.Compare(a.Field, b.Field) })
	return domain.Validation(unknown...)
}

// requirement returns the authz requirement of a route.
func (rt Route) requirement() authz.Requirement {
	return authz.Requirement{Permission: rt.Permission, AnyOf: rt.AnyOf, Condition: rt.Condition}
}
