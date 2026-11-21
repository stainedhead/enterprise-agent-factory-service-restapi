package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// IfMatchRules reads from the OpenAPI document which operations require an If-Match header (the
// IfMatch parameter, on PATCH and DELETE). The PUTs that may create their resource declare
// IfMatchPut instead, and their handlers answer 428 only when the resource exists (CT-API-6).
func IfMatchRules(doc []byte) (map[string]bool, error) {
	var d struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("middleware: read OpenAPI: %w", err)
	}
	required := map[string]bool{}
	for _, item := range d.Paths {
		for method, node := range item {
			if method == "parameters" {
				continue
			}
			var op struct {
				OperationID string `yaml:"operationId"`
				Parameters  []struct {
					Ref string `yaml:"$ref"`
				} `yaml:"parameters"`
			}
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("middleware: read OpenAPI operation: %w", err)
			}
			for _, p := range op.Parameters {
				if p.Ref == "#/components/parameters/IfMatch" {
					required[op.OperationID] = true
				}
			}
		}
	}
	return required, nil
}

// RequireIfMatch answers 428 PreconditionRequired for a request to an operation that requires If-Match
// and carries none, and 400 ValidationError for one that is not a version. The version itself is
// compared by the use case against the row it locks (a stale value is 412 there). It runs after
// authorization, so a caller who may not know the resource exists gets 404 or 403, never 428.
func RequireIfMatch(required map[string]bool, routes RouteLookup, errs ErrorWriter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if info, ok := routes(r); ok && required[info.OperationID] {
				if _, err := domain.ParseIfMatch(r.Header.Get("If-Match")); err != nil {
					errs(w, r, err)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// IfMatch parses the If-Match header of r. present is false when there is none (err nil); a header
// that is not a single version is a ValidationError.
func IfMatch(r *http.Request) (v domain.Version, present bool, err error) {
	h := r.Header.Get("If-Match")
	if h == "" {
		return 0, false, nil
	}
	v, err = domain.ParseIfMatch(h)
	return v, err == nil, err
}

// CheckIfMatch is the whole precondition for an update of an existing resource at version current:
// 428 when the header is missing, 400 when it is malformed, 412 when it is stale.
func CheckIfMatch(r *http.Request, current domain.Version) error {
	want, err := domain.ParseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		return err
	}
	return domain.CheckVersion(current, want)
}

// SetETag sets the ETag header for version v.
func SetETag(w http.ResponseWriter, v domain.Version) { w.Header().Set("ETag", v.ETag()) }

// maxETagBody bounds how much of a response is buffered to find its version.
const maxETagBody = 256 << 10

// AutoETag gives every successful JSON response whose top-level object has an integer "version" the
// header ETag: "<version>" (CT-API-13), unless the handler already set one. Lists and anything larger
// than 256 KiB pass unchanged. It makes the rule hold for every resource without each handler
// remembering it.
func AutoETag() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead || r.Method == http.MethodDelete {
				next.ServeHTTP(w, r)
				return
			}
			bw := &bufferedWriter{ResponseWriter: w, limit: maxETagBody}
			next.ServeHTTP(bw, r)
			bw.finish(func(h http.Header, status int, body []byte) {
				if status/100 == 2 && h.Get("ETag") == "" && isJSON(h) {
					var probe struct {
						Version *json.Number `json:"version"`
					}
					dec := json.NewDecoder(bytes.NewReader(body))
					dec.UseNumber()
					if dec.Decode(&probe) == nil && probe.Version != nil {
						if n, err := strconv.ParseInt(probe.Version.String(), 10, 64); err == nil && n >= 1 {
							h.Set("ETag", domain.Version(n).ETag())
						}
					}
				}
			})
		})
	}
}

func isJSON(h http.Header) bool {
	ct := h.Get("Content-Type")
	return len(ct) >= 16 && ct[:16] == "application/json"
}
