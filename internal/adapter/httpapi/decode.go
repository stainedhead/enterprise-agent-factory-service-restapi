package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// DefaultBodyLimit is the request body limit of every route without an override (1 MiB).
const DefaultBodyLimit = 1 << 20

// bodyLimits overrides DefaultBodyLimit per operation (CreateImport takes bundles up to 100 MB).
var bodyLimits = map[string]int64{
	"CreateImport": 100 << 20,
}

func bad(field, reason string) error {
	return domain.Validation(domain.FieldError{Field: field, Reason: reason})
}

// Decode reads a JSON body into dst. Unknown fields, trailing data and an oversized body are
// ValidationErrors. On a team-scoped route, a body teamId that differs from the path's is a
// ValidationError: the team always comes from the path (RA-US-1.6).
func Decode(r *http.Request, dst any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return bad("body", "request body is larger than the limit")
		}
		return bad("body", "unreadable request body")
	}
	if pathTeam := r.PathValue("teamId"); pathTeam != "" {
		var probe struct {
			TeamID *string `json:"teamId"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.TeamID != nil && *probe.TeamID != pathTeam {
			return bad("teamId", "must match the team in the path")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return bad(jsonField(err), "invalid JSON: "+err.Error())
	}
	if dec.More() {
		return bad("body", "trailing data after the JSON document")
	}
	return nil
}

func jsonField(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return te.Field
	}
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		return strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
	}
	return "body"
}
