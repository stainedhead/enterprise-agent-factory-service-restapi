// Package contract replays the OpenAPI examples of every operation against an in-process afs-api
// on a seeded database and validates each request and response with libopenapi-validator
// (M0-D6, CT-API-7). Operations still on their 501 placeholder are counted, not validated; every
// other response, including the 401 of an unauthenticated replay, must match the document.
package contract

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	pyaml "github.com/pb33f/go-yaml"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	vErrors "github.com/pb33f/libopenapi-validator/errors"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var pathParamRE = regexp.MustCompile(`\{[A-Za-z]+\}`)

func server(t *testing.T) http.Handler {
	t.Helper()
	db := pgtest.Get(t)
	db.SeedTeam(t)
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_app"], Service: "afs-api"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, err := httpapi.New(httpapi.Config{AllowUnbound: true, Ready: map[string]httpapi.ReadinessCheck{
		"database": func(r *http.Request) error { return pool.Ping(r.Context()) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestReplayExamples(t *testing.T) {
	h := server(t)
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	val, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	m, err := doc.BuildV3Model()
	if err != nil {
		t.Fatal(err)
	}
	var total, placeholders, validated int
	for pair := m.Model.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		path, item := pair.Key(), pair.Value()
		verb := strings.Contains(path, "}:")
		for op := item.GetOperations().First(); op != nil; op = op.Next() {
			method, o := strings.ToUpper(op.Key()), op.Value()
			total++
			t.Run(o.OperationId, func(t *testing.T) {
				req := exampleRequest(t, method, path, o)
				var ok bool
				var verrs []*vErrors.ValidationError
				if verb {
					ok, verrs = val.ValidateHttpRequestSyncWithPathItem(req, item, path)
				} else {
					ok, verrs = val.ValidateHttpRequestSync(req)
				}
				if !ok {
					for _, e := range verrs {
						t.Errorf("request: %s %s", e.Message, e.Reason)
					}
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, exampleRequest(t, method, path, o))
				if rec.Code == http.StatusNotImplemented && strings.Contains(rec.Body.String(), `"not_implemented"`) {
					placeholders++
					return
				}
				resp := rec.Result()
				defer func() { _ = resp.Body.Close() }()
				vreq := exampleRequest(t, method, path, o)
				if verb {
					ok, verrs = val.GetResponseBodyValidator().ValidateResponseBodyWithPathItem(vreq, resp, item, path)
				} else {
					ok, verrs = val.ValidateHttpResponse(vreq, resp)
				}
				if !ok {
					for _, e := range verrs {
						t.Errorf("response %d: %s %s (%s)", rec.Code, e.Message, e.Reason, rec.Body.String())
					}
				}
				for _, hdr := range []string{"X-Request-Id", "X-Afs-Contract-Version"} {
					if rec.Header().Get(hdr) == "" {
						t.Errorf("response %d has no %s", rec.Code, hdr)
					}
				}
				validated++
			})
		}
	}
	if total != 182 {
		t.Fatalf("operations = %d, want 182", total)
	}
	t.Logf("%d operations: %d responses validated, %d on placeholders", total, validated, placeholders)
}

func exampleRequest(t *testing.T, method, path string, o *v3.Operation) *http.Request {
	t.Helper()
	values := map[string]string{}
	for _, p := range o.Parameters {
		if p.In == "path" {
			s := p.Schema.Schema()
			values[p.Name] = paramExample(t, s.Example, s.Enum, s.Pattern, p.Name)
		}
	}
	u := pathParamRE.ReplaceAllStringFunc(path, func(s string) string { return values[s[1:len(s)-1]] })
	var body io.Reader
	if o.RequestBody != nil {
		mt := o.RequestBody.Content.GetOrZero("application/json")
		if mt == nil || mt.Example == nil {
			t.Fatal("request body has no example")
		}
		var v any
		if err := mt.Example.Decode(&v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, "https://afs.example.com"+u, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer example")
	for _, p := range o.Parameters {
		if p.In == "header" && p.Required != nil && *p.Required {
			req.Header.Set(p.Name, `"3"`)
		}
	}
	return req
}

func paramExample(t *testing.T, ex *pyaml.Node, enum []*pyaml.Node, pattern, name string) string {
	t.Helper()
	switch {
	case ex != nil:
		return ex.Value
	case len(enum) > 0:
		return enum[0].Value
	case name == "name":
		return "LOG_LEVEL"
	case strings.HasPrefix(pattern, "^nch_"), strings.HasPrefix(pattern, "^log_"), strings.HasPrefix(pattern, "^idp_"):
		return pattern[1:strings.Index(pattern, "_")+1] + "01JD2Q3R4S5T6V7W8X9Y0Z0001"
	}
	t.Fatalf("path parameter %s has no example", name)
	return ""
}
