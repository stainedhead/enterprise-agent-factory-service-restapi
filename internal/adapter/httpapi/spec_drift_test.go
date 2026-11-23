package httpapi

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
)

// specSchema is the slice of an OpenAPI schema the drift guard reads.
type specSchema struct {
	Ref        string                `yaml:"$ref"`
	AllOf      []specSchema          `yaml:"allOf"`
	Properties map[string]specSchema `yaml:"properties"`
	Required   []string              `yaml:"required"`
}

// specProps returns the property names of s (following $ref and allOf) and the schema of each.
func specProps(all map[string]specSchema, s specSchema) map[string]specSchema {
	out := map[string]specSchema{}
	if s.Ref != "" {
		s = all[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
	}
	for _, a := range s.AllOf {
		for k, v := range specProps(all, a) {
			out[k] = v
		}
	}
	for k, v := range s.Properties {
		out[k] = v
	}
	return out
}

// jsonFields lists the JSON members a Go struct (or pointer to one) decodes, with the Go type of each.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]reflect.Type{}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = t.Field(i).Type
		}
	}
	return out
}

func compareProps(t *testing.T, all map[string]specSchema, path string, spec map[string]specSchema, ft reflect.Type) {
	t.Helper()
	fields := jsonFields(ft)
	for name := range spec {
		if _, ok := fields[name]; !ok {
			t.Errorf("%s.%s is in api/openapi.yaml but the handler rejects it (unknown field)", path, name)
		}
	}
	for name, typ := range fields {
		sub, ok := spec[name]
		if !ok {
			t.Errorf("%s.%s is decoded by the handler but missing from api/openapi.yaml", path, name)
			continue
		}
		if sp := specProps(all, sub); len(sp) > 0 {
			compareProps(t, all, path+"."+name, sp, typ)
		}
	}
}

// TestRequestBodiesMatchSpec is the guard against drift between the handlers' strictly decoded request
// bodies (DisallowUnknownFields) and the request schemas of api/openapi.yaml: both directions, nested
// objects included. A new request struct with a schema of the same name goes in the table.
func TestRequestBodiesMatchSpec(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]specSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	all := doc.Components.Schemas
	cases := map[string]any{
		"CreateTeamRequest": createTeamRequest{}, "UpdateTeamRequest": updateTeamRequest{},
		"CreateAgentRequest": createAgentRequest{}, "UpdateAgentRequest": updateAgentRequest{},
		"PutBudgetRequest": putBudgetRequest{}, "CreateStageRequest": createStageRequest{}, "UpdateStageRequest": updateStageRequest{},
		"CreatePoolRequest": createPoolRequest{}, "UpdatePoolRequest": updatePoolRequest{},
		"CreateLogDestinationRequest": createLogDestinationRequest{}, "UpdateLogDestinationRequest": updateLogDestinationRequest{},
		"CreateReportScheduleRequest": createScheduleRequest{},
		"CreateSecretRequest":         createSecretRequest{}, "UpdateSecretRequest": updateSecretRequest{},
		"CreateWorkOrderRequest": createWorkOrderRequest{}, "UpdateWorkOrderRequest": updateWorkOrderRequest{},
		"UpdateWorkOrderRepoRequest":  updateWorkOrderRepoRequest{},
		"PutTeamCollaborationRequest": putCollaborationRequest{}, "PutTeamOrchestrationRequest": putOrchestrationRequest{},
		"ChangeCollaborationToolRequest": changeToolRequest{}, "ConnectAgentRequest": connectRequest{},
		"RotateCollaborationKeysRequest": rotateRequest{},
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			s, ok := all[n]
			if !ok {
				t.Fatalf("schema %s is not in api/openapi.yaml", n)
			}
			compareProps(t, all, n, specProps(all, s), reflect.TypeOf(cases[n]))
		})
	}
}

// TestResponseBodiesMatchSpec: a response struct emits only members its schema documents, and emits every member
// the schema requires.
func TestResponseBodiesMatchSpec(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]specSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	all := doc.Components.Schemas
	cases := map[string]any{
		"Team": teamBody{}, "Setting": settingBody{}, "Agent": agentBody{}, "Pool": poolBody{}, "Budget": budgetBody{},
		"Secret": secretBody{}, "WorkOrder": workOrderBody{}, "Member": memberBody{}, "TeamOrchestration": teamOrchestrationBody{},
		"OrchestrationEndpoint": orchestrationEndpointBody{}, "TeamCollaboration": teamCollaborationBody{},
		"Model": modelBody{}, "AgentTemplate": agentTemplateBody{}, "SwarmTemplate": swarmTemplateBody{},
		"IdentityProvider": idpBody{}, "ServiceTag": serviceTagBody{}, "ApiKey": apiKeyBody{}, "Export": exportBody{},
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			s, ok := all[n]
			if !ok {
				t.Fatalf("schema %s is not in api/openapi.yaml", n)
			}
			spec := specProps(all, s)
			fields := jsonFields(reflect.TypeOf(cases[n]))
			for name := range fields {
				if _, ok := spec[name]; !ok {
					t.Errorf("%s.%s is emitted by the handler but missing from api/openapi.yaml", n, name)
				}
			}
			req := slices.Clone(s.Required)
			for _, a := range s.AllOf {
				req = append(req, a.Required...)
			}
			for _, name := range req {
				if _, ok := fields[name]; !ok {
					t.Errorf("%s.%s is required by api/openapi.yaml but the handler never emits it", n, name)
				}
			}
		})
	}
}
