package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	pyaml "github.com/pb33f/go-yaml"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	vErrors "github.com/pb33f/libopenapi-validator/errors"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"gopkg.in/yaml.v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type contractOp struct {
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Permission  string `json:"permission"`
	Scope       string `json:"scope"`
	CSV         bool   `json:"csv"`
}

func loadOps(t *testing.T) []contractOp {
	t.Helper()
	raw, err := os.ReadFile("contract/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	var ops []contractOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

func model(t *testing.T) *v3.Document {
	t.Helper()
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	m, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("build model: %v", err)
	}
	return &m.Model
}

// raw decodes the bundle as plain YAML for structural checks.
func raw(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func operations(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	paths := raw(t)["paths"].(map[string]any)
	for p, item := range paths {
		for method, op := range item.(map[string]any) {
			o := op.(map[string]any)
			o["__path"] = p
			o["__method"] = strings.ToUpper(method)
			out[o["operationId"].(string)] = o
		}
	}
	return out
}

// TestDocumentValid: the bundle is a valid OpenAPI 3.1 document with the contract version.
func TestDocumentValid(t *testing.T) {
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if v := doc.GetVersion(); v != "3.1.0" {
		t.Errorf("openapi = %s", v)
	}
	val, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatalf("validator: %v", errs)
	}
	if ok, verrs := val.ValidateDocument(); !ok {
		for _, e := range verrs {
			t.Errorf("document: %s", e.Error())
		}
	}
	if got := model(t).Info.Version; got != api.ContractVersion {
		t.Errorf("info.version = %s, want %s", got, api.ContractVersion)
	}
}

// TestOperationsEqualContract: operationIds, methods, paths, permissions and scopes equal the
// contract extract (all 276 operations).
func TestOperationsEqualContract(t *testing.T) {
	ops := loadOps(t)
	if len(ops) != 276 {
		t.Fatalf("contract operations = %d", len(ops))
	}
	got := operations(t)
	if len(got) != len(ops) {
		t.Errorf("OpenAPI operations = %d, want %d", len(got), len(ops))
	}
	for _, c := range ops {
		o, ok := got[c.OperationID]
		if !ok {
			t.Errorf("%s missing from the OpenAPI document", c.OperationID)
			continue
		}
		perm := c.Permission
		if perm == "" {
			perm = map[string]string{"public": "none", "any": "any signed-in"}[c.Scope]
		}
		if o["__method"] != c.Method || o["__path"] != c.Path || o["x-afs-permission"] != perm || o["x-afs-scope"] != c.Scope || o["x-afs-csv"] != c.CSV {
			t.Errorf("%s = %v %v %v %v csv=%v, want %s %s %s %s csv=%v", c.OperationID, o["__method"], o["__path"],
				o["x-afs-permission"], o["x-afs-scope"], o["x-afs-csv"], c.Method, c.Path, perm, c.Scope, c.CSV)
		}
		for _, ext := range []string{"x-afs-cli", "x-afs-sdk", "x-afs-screen", "x-afs-priority", "x-afs-events"} {
			if _, ok := o[ext]; !ok {
				t.Errorf("%s has no %s", c.OperationID, ext)
			}
		}
	}
}

// TestErrorCodes: only API contract §6 codes that afs-api returns are documented; never 402 or 502.
func TestErrorCodes(t *testing.T) {
	var errs []struct {
		HTTP        int    `json:"http"`
		Code        string `json:"code"`
		GatewayOnly bool   `json:"gatewayOnly"`
	}
	b, err := os.ReadFile("contract/errors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &errs); err != nil {
		t.Fatal(err)
	}
	var want []string
	statusOf := map[string]int{}
	for _, e := range errs {
		if !e.GatewayOnly {
			want = append(want, e.Code)
			statusOf[e.Code] = e.HTTP
		}
	}
	sort.Strings(want)
	m := model(t)
	sch := m.Components.Schemas.GetOrZero("ErrorCode").Schema()
	var got []string
	for _, e := range sch.Enum {
		got = append(got, e.Value)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("ErrorCode enum = %v, want %v", got, want)
	}
	for name, ops := range operations(t) {
		for code := range ops["responses"].(map[string]any) {
			if code == "402" || code == "502" {
				t.Errorf("%s documents %s, which only afs-gateway returns", name, code)
			}
		}
	}
	for _, r := range []struct{ resp, code string }{{"BadRequest", "ValidationError"}, {"Unauthenticated", "Unauthenticated"},
		{"AccessDenied", "AccessDenied"}, {"NotFound", "NotFound"}, {"Conflict", "Conflict"}, {"PreconditionFailed", "PreconditionFailed"},
		{"PreconditionRequired", "PreconditionRequired"}, {"Throttled", "Throttled"}, {"InternalError", "InternalError"}, {"Unavailable", "Unavailable"}} {
		resp := raw(t)["components"].(map[string]any)["responses"].(map[string]any)[r.resp].(map[string]any)
		ex := resp["content"].(map[string]any)["application/json"].(map[string]any)["example"].(map[string]any)
		if ex["code"] != r.code || statusOf[r.code] == 0 {
			t.Errorf("response %s example code = %v", r.resp, ex["code"])
		}
	}
}

// TestStandardResponses: every operation documents its error responses, including 428 on PATCH,
// PUT and DELETE, and success responses carry X-Request-Id and X-Afs-Contract-Version.
func TestStandardResponses(t *testing.T) {
	for name, o := range operations(t) {
		rs := o["responses"].(map[string]any)
		want := []string{"400", "429", "500"}
		if o["x-afs-scope"] != "public" {
			want = append(want, "401", "403", "503")
		}
		switch o["__method"] {
		case "PATCH", "PUT", "DELETE":
			want = append(want, "409", "412", "428")
		case "POST":
			want = append(want, "409")
		}
		for _, c := range want {
			if _, ok := rs[c]; !ok {
				t.Errorf("%s lacks a %s response", name, c)
			}
		}
		for code, r := range rs {
			if !strings.HasPrefix(code, "2") {
				continue
			}
			h, _ := r.(map[string]any)["headers"].(map[string]any)
			if h["X-Request-Id"] == nil || h["X-Afs-Contract-Version"] == nil {
				t.Errorf("%s %s lacks the standard headers", name, code)
			}
		}
	}
}

var replicaRE = regexp.MustCompile(`(?i)^(replicas?|replicaCount|desiredReplicas)$`)

// TestNoReplicaCount: no schema property is a replica count (RA-US-4.6).
func TestNoReplicaCount(t *testing.T) {
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if props, ok := x["properties"].(map[string]any); ok {
				for k := range props {
					if replicaRE.MatchString(k) {
						t.Errorf("%s.%s is a replica count", path, k)
					}
				}
			}
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("", raw(t))
}

// TestEnumsMatchDomain: the OpenAPI enums of contract §5 equal internal/domain's enumerations.
func TestEnumsMatchDomain(t *testing.T) {
	m := model(t)
	checks := []struct{ schema, prop, enum string }{
		{"Team", "kind", "TeamKind"}, {"Team", "state", "TeamState"}, {"Team", "isolationTier", "IsolationTier"},
		{"Team", "teardownRule", "TeardownRule"}, {"Team", "teardownReason", "TeardownReason"}, {"Member", "role", "TeamRole"},
		{"Member", "subjectType", "SubjectType"}, {"Principal", "type", "PrincipalType"}, {"SecretBinding", "delivery", "SecretDelivery"},
		{"Agent", "iamIdentity", "IAMIdentity"}, {"AgentVolume", "onDelete", "VolumeOnDelete"}, {"AgentVolume", "snapshotSchedule", "SnapshotSchedule"},
		{"RoleLink", "status", "RoleLinkStatus"}, {"Pool", "state", "PoolState"}, {"Pool", "sharedVolume", "SharedVolume"},
		{"WorkOrder", "kind", "WorkOrderKind"}, {"WorkOrder", "severity", "Severity"}, {"WorkOrder", "status", "WorkOrderStatus"},
		{"WorkOrder", "slaState", "SLAState"}, {"WorkOrder", "source", "WorkOrderSource"}, {"WorkOrderRepo", "status", "RepoStatus"},
		{"WorkOrderRepo", "ownerSource", "OwnerSource"}, {"ReportSchedule", "report", "ReportKind"}, {"ReportSchedule", "cadence", "Cadence"},
		{"ReportSchedule", "format", "ReportFormat"}, {"TeamTag", "source", "TagSource"}, {"Budget", "period", "BudgetPeriod"},
		{"Budget", "onExhaust", "OnExhaust"}, {"IdentityProvider", "kind", "IdPKind"}, {"Export", "status", "ExportStatus"},
		{"PriceProposal", "status", "PriceProposalStatus"}, {"CostTagStatus", "awsStatus", "CostTagAWSStatus"}, {"Secret", "scope", "SecretScope"},
		{"ConfigValue", "source", "ConfigSource"}, {"ExternalSystem", "scope", "ExtSystemScope"}, {"ExternalSystem", "teamState", "ExtSystemTeamState"},
		{"ExternalSystem", "disabledAt", "ExtSystemDisabledAt"}, {"ServiceTag", "mode", "ServiceTagMode"}, {"ImportTeamResult", "result", "ImportResult"},
		{"SwarmTemplate", "kind", "SwarmTemplateKind"}, {"AuditEvent", "actorType", "ActorType"}, {"AuditEvent", "outcome", "AuditOutcome"},
	}
	for _, c := range checks {
		sp := m.Components.Schemas.GetOrZero(c.schema)
		if sp == nil {
			t.Errorf("schema %s missing", c.schema)
			continue
		}
		prop := sp.Schema().Properties.GetOrZero(c.prop)
		if prop == nil {
			t.Errorf("%s.%s missing", c.schema, c.prop)
			continue
		}
		var got []string
		for _, e := range prop.Schema().Enum {
			if e.Tag != "!!null" {
				got = append(got, e.Value)
			}
		}
		want := domain.EnumValues[c.enum]
		// OpenAPI-only values: the root contract does not list them yet (CD-11, pool views).
		if c.schema == "ExternalSystem" && c.prop == "disabledAt" {
			want = append(slices.Clone(want), string(domain.ExtSystemDisabledAtPool))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s.%s enum = %v, want domain.%s %v", c.schema, c.prop, got, c.enum, want)
		}
	}
}

var pathParamRE = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// TestExamplesValidate: every operation has its examples, and each request and response example
// validates against the document (requests and responses built from them, as the contract test of
// test/contract replays them against the server).
func TestExamplesValidate(t *testing.T) {
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	val, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	m := model(t)
	for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		path := pair.Key()
		for op := pair.Value().GetOperations().First(); op != nil; op = op.Next() {
			method, o := strings.ToUpper(op.Key()), op.Value()
			t.Run(o.OperationId, func(t *testing.T) {
				// The validator's router does not match action paths ({id}:verb); give it the path item.
				verb := strings.Contains(path, "}:")
				item := pair.Value()
				req := exampleRequest(t, method, path, o)
				ok, verrs := false, []*vErrors.ValidationError(nil)
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
				checked := 0
				for rp := o.Responses.Codes.First(); rp != nil; rp = rp.Next() {
					code := rp.Key()
					if strings.HasPrefix(code, "3") {
						checked++ // a redirect (CompleteMcpOAuthConnect, CD-48) has no body to validate
						continue
					}
					if !strings.HasPrefix(code, "2") {
						continue
					}
					r := rp.Value()
					if r.Content == nil || r.Content.Len() == 0 {
						checked++
						continue
					}
					for mt := r.Content.First(); mt != nil; mt = mt.Next() {
						if mt.Value().Example == nil {
							t.Errorf("%s %s has no example", code, mt.Key())
							continue
						}
						resp := exampleResponse(t, code, mt.Key(), mt.Value().Example)
						defer func() { _ = resp.Body.Close() }()
						req := exampleRequest(t, method, path, o)
						req.Header.Set("Accept", mt.Key())
						var ok bool
						var verrs []*vErrors.ValidationError
						if verb {
							ok, verrs = val.GetResponseBodyValidator().ValidateResponseBodyWithPathItem(req, resp, item, path)
						} else {
							ok, verrs = val.ValidateHttpResponse(req, resp)
						}
						if !ok {
							for _, e := range verrs {
								t.Errorf("response %s %s: %s %s", code, mt.Key(), e.Message, e.Reason)
							}
						}
						checked++
					}
				}
				if checked == 0 {
					t.Error("no success response")
				}
			})
		}
	}
}

func exampleRequest(t *testing.T, method, path string, o *v3.Operation) *http.Request {
	t.Helper()
	values := map[string]string{}
	for _, p := range o.Parameters {
		if p.In == "path" {
			values[p.Name] = paramExample(t, p.Schema.Schema().Example, p.Schema.Schema().Enum, p.Schema.Schema().Pattern, p.Name)
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

func exampleResponse(t *testing.T, code, mediaType string, ex *pyaml.Node) *http.Response {
	t.Helper()
	var body []byte
	if mediaType == "application/json" {
		var v any
		if err := ex.Decode(&v); err != nil {
			t.Fatal(err)
		}
		body, _ = json.Marshal(v)
	} else {
		body = []byte(ex.Value)
	}
	var status int
	_, _ = fmt.Sscanf(code, "%d", &status)
	h := http.Header{}
	h.Set("Content-Type", mediaType)
	h.Set("X-Request-Id", "req_1")
	h.Set("X-Afs-Contract-Version", api.ContractVersion)
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body))}
}

// paramEnum returns the enum of the named parameter of an operation.
func paramEnum(t *testing.T, opID, name string) []string {
	t.Helper()
	for _, p := range operations(t)[opID]["parameters"].([]any) {
		pm := p.(map[string]any)
		if pm["name"] != name {
			continue
		}
		var out []string
		for _, e := range pm["schema"].(map[string]any)["enum"].([]any) {
			out = append(out, e.(string))
		}
		return out
	}
	t.Fatalf("%s has no parameter %s", opID, name)
	return nil
}

// TestSettingsMatchRegistry: the PutSetting key enum is the registry of settings afs-api accepts (every key of
// domain.AllSettingSpecs except the read-only deprecated aliases), and Setting.type covers every registry type.
func TestSettingsMatchRegistry(t *testing.T) {
	var want []string
	types := map[string]bool{}
	for _, s := range domain.AllSettingSpecs() {
		types[s.Type] = true
		if _, alias := domain.RenamedSettings[s.Key]; !alias {
			want = append(want, s.Key)
		}
	}
	got := paramEnum(t, "PutSetting", "key")
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("PutSetting key enum = %v, want %v", got, want)
	}
	for _, k := range got {
		if _, alias := domain.RenamedSettings[k]; alias {
			t.Errorf("PutSetting key enum lists the read-only alias %s", k)
		}
	}
	setting := model(t).Components.Schemas.GetOrZero("Setting").Schema().Properties.GetOrZero("type").Schema()
	have := map[string]bool{}
	for _, e := range setting.Enum {
		have[e.Value] = true
	}
	for ty := range types {
		if !have[ty] {
			t.Errorf("Setting.type enum lacks %q, which the registry uses", ty)
		}
	}
	// The deprecated aliases are documented where a client looks for them.
	d := operations(t)["PutSetting"]["description"].(string)
	for old, next := range domain.RenamedSettings {
		if !strings.Contains(d, old) || !strings.Contains(d, next) || !strings.Contains(d, "setting_renamed") {
			t.Errorf("PutSetting description does not document %s -> %s (setting_renamed)", old, next)
		}
	}
}

// TestAgentRoleFilterMatchesDomain: the ListAgents role filter takes every role the handler accepts, the
// deprecated collaboration alias included.
func TestAgentRoleFilterMatchesDomain(t *testing.T) {
	var want []string
	for _, r := range []domain.AgentRole{domain.AgentRoleWorker, domain.AgentRoleOrchestration, domain.AgentRoleCollaboration} {
		if !r.Valid() {
			t.Fatalf("role %s is not valid in the domain", r)
		}
		want = append(want, string(r))
	}
	if got := paramEnum(t, "ListAgents", "role"); !slices.Equal(got, want) {
		t.Errorf("ListAgents role enum = %v, want %v", got, want)
	}
}
