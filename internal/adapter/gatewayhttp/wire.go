package gatewayhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// wireKind is the request format of a path.
type wireKind int

const (
	wireConverse wireKind = iota
	wireInvoke
	wireAnthropic
)

// anthropicVersion is the version string Bedrock requires in an Anthropic Messages body.
const anthropicVersion = "bedrock-2023-05-31"

// parsed is a request the adapter has read and checked: what the use case needs (model, limit) and what
// the adapter needs to build the upstream body.
type parsed struct {
	wire    wireKind
	op      gateway.Operation
	rtOp    bedrock.Operation
	modelID string
	stream  bool
	// maxTokens is the request's own output limit, zero when it sent none (or an unusable one); capPath says
	// where the limit lives or, when absent, where the gateway writes it.
	maxTokens int
	capPath   []string
	doc       map[string]json.RawMessage
	beta      string
}

func badRequest(field, reason string) error {
	return domain.Validation(domain.FieldError{Field: field, Reason: reason})
}

func parseDoc(body []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return nil, badRequest("body", "must be a JSON object")
	}
	return doc, nil
}

// pathInt reads a positive integer at a path of object keys.
func pathInt(doc map[string]json.RawMessage, path ...string) (int, bool) {
	if len(path) == 0 {
		return 0, false
	}
	raw, ok := doc[path[0]]
	if !ok {
		return 0, false
	}
	if len(path) > 1 {
		var sub map[string]json.RawMessage
		if json.Unmarshal(raw, &sub) != nil {
			return 0, false
		}
		return pathInt(sub, path[1:]...)
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < 1 || n > 1<<31 {
		return 0, false
	}
	return int(n), true
}

// setInt writes an integer at a path of object keys, creating the objects on the way when they are
// absent or null; anything else on the way is an error.
func setInt(doc map[string]json.RawMessage, v int, path ...string) error {
	if len(path) == 1 {
		doc[path[0]] = json.RawMessage(fmt.Sprint(v))
		return nil
	}
	var sub map[string]json.RawMessage
	if raw, ok := doc[path[0]]; ok {
		if err := json.Unmarshal(raw, &sub); err != nil {
			return err
		}
	}
	if sub == nil {
		sub = map[string]json.RawMessage{}
	}
	if err := setInt(sub, v, path[1:]...); err != nil {
		return err
	}
	b, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	doc[path[0]] = b
	return nil
}

// invokeLimits are the output limit fields of the Bedrock model families the gateway can meter.
var invokeLimits = [][]string{
	{"max_tokens"}, {"max_tokens_to_sample"}, {"max_gen_len"}, {"textGenerationConfig", "maxTokenCount"},
}

// limitPathOf returns the output limit field of the Bedrock model family of a model id, nil when the
// gateway does not know the family (FR-R04, Q3). The id may carry a cross-region prefix (us., eu.,
// apac., global.) or be a foundation model ARN.
func limitPathOf(modelID string) []string {
	id := strings.ToLower(modelID[strings.LastIndex(modelID, "/")+1:])
	parts := strings.Split(id, ".")
	for i, vendor := range parts {
		rest := strings.Join(parts[i+1:], ".")
		switch vendor {
		case "anthropic":
			if strings.HasPrefix(rest, "claude-v2") || strings.HasPrefix(rest, "claude-instant") {
				return []string{"max_tokens_to_sample"}
			}
			return []string{"max_tokens"}
		case "amazon":
			if strings.HasPrefix(rest, "titan-text") {
				return []string{"textGenerationConfig", "maxTokenCount"}
			}
		case "meta":
			return []string{"max_gen_len"}
		case "mistral", "cohere":
			return []string{"max_tokens"}
		}
	}
	return nil
}

func parseConverse(rtOp bedrock.Operation, op gateway.Operation, modelID string, body []byte) (*parsed, error) {
	if !gateway.ValidModelID(modelID) {
		return nil, badRequest("modelId", "is not a valid model id")
	}
	doc, err := parseDoc(body)
	if err != nil {
		return nil, err
	}
	p := &parsed{wire: wireConverse, op: op, rtOp: rtOp, modelID: modelID, stream: op.Streams(), doc: doc}
	p.capPath = []string{"inferenceConfig", "maxTokens"}
	p.maxTokens, _ = pathInt(doc, p.capPath...)
	return p, nil
}

func parseInvoke(rtOp bedrock.Operation, op gateway.Operation, modelID string, body []byte) (*parsed, error) {
	if !gateway.ValidModelID(modelID) {
		return nil, badRequest("modelId", "is not a valid model id")
	}
	doc, err := parseDoc(body)
	if err != nil {
		return nil, err
	}
	p := &parsed{wire: wireInvoke, op: op, rtOp: rtOp, modelID: modelID, stream: op.Streams(), doc: doc}
	for _, path := range invokeLimits {
		if n, ok := pathInt(doc, path...); ok {
			p.maxTokens, p.capPath = n, path
			break
		}
	}
	if p.capPath == nil {
		// The body names no limit: set the family's field to the cap, or refuse when it is not known.
		if p.capPath = limitPathOf(modelID); p.capPath == nil {
			return nil, badRequest("body", "has no output limit and the gateway cannot set one for this model family; "+
				"send the model's own output limit (max_tokens, max_gen_len or textGenerationConfig.maxTokenCount)")
		}
	}
	return p, nil
}

// parseAnthropic reads an Anthropic Messages request. The Bedrock body is the same minus "model" and
// "stream", plus the Bedrock version string (and the beta header as "anthropic_beta").
func parseAnthropic(body []byte, h http.Header) (*parsed, error) {
	doc, err := parseDoc(body)
	if err != nil {
		return nil, err
	}
	var model string
	if json.Unmarshal(doc["model"], &model) != nil || !gateway.ValidModelID(model) {
		return nil, badRequest("model", "is required and must be a valid model id")
	}
	var stream bool
	if raw, ok := doc["stream"]; ok && json.Unmarshal(raw, &stream) != nil {
		return nil, badRequest("stream", "must be a boolean")
	}
	// The Messages API requires max_tokens; a request without one is refused here, never forwarded uncapped.
	n, ok := pathInt(doc, "max_tokens")
	if !ok {
		return nil, badRequest("max_tokens", "is required and must be a positive integer")
	}
	p := &parsed{wire: wireAnthropic, op: gateway.OpAnthropicMessages, modelID: model, stream: stream, doc: doc,
		maxTokens: n, capPath: []string{"max_tokens"}, beta: strings.TrimSpace(h.Get("Anthropic-Beta"))}
	p.rtOp = bedrock.OpInvoke
	if stream {
		p.rtOp = bedrock.OpInvokeWithResponseStream
	}
	return p, nil
}

// upstreamBody builds the body sent to Bedrock: the agent's own, with the output limit capped to the
// model's, and for Converse the requestMetadata replaced by the gateway's (gateway contract §4, §6).
func (p *parsed) upstreamBody(a gateway.Authorized) ([]byte, error) {
	doc := make(map[string]json.RawMessage, len(p.doc)+2)
	for k, v := range p.doc {
		doc[k] = v
	}
	// The limit sent is the reserved cap whenever the request sent none or more (FR-R04): the amount held
	// and the tokens Bedrock may produce are the same number.
	if a.MaxOutputTokens > 0 && (p.maxTokens == 0 || p.maxTokens > a.MaxOutputTokens) {
		if err := setInt(doc, a.MaxOutputTokens, p.capPath...); err != nil {
			return nil, badRequest(strings.Join(p.capPath, "."), "is not an object path")
		}
	}
	switch p.wire {
	case wireConverse:
		md, err := json.Marshal(a.Metadata)
		if err != nil {
			return nil, domain.Internal(err)
		}
		doc["requestMetadata"] = md
	case wireAnthropic:
		delete(doc, "model")
		delete(doc, "stream")
		if _, ok := doc["anthropic_version"]; !ok {
			doc["anthropic_version"] = json.RawMessage(`"` + anthropicVersion + `"`)
		}
		if p.beta != "" {
			if _, ok := doc["anthropic_beta"]; !ok {
				b, _ := json.Marshal(strings.Split(strings.ReplaceAll(p.beta, " ", ""), ","))
				doc["anthropic_beta"] = b
			}
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, domain.Internal(err)
	}
	return b, nil
}

type usageJSON struct {
	InputTokens   *int `json:"inputTokens"`
	OutputTokens  *int `json:"outputTokens"`
	CacheRead     *int `json:"cacheReadInputTokens"`
	CacheWrite    *int `json:"cacheWriteInputTokens"`
	AInput        *int `json:"input_tokens"`
	AOutput       *int `json:"output_tokens"`
	ACacheRead    *int `json:"cache_read_input_tokens"`
	ACacheCreated *int `json:"cache_creation_input_tokens"`
}

func val(p *int) int {
	if p == nil {
		return gateway.Absent
	}
	return *p
}

// usage returns the usage of either spelling; ok is false when the object reports none.
func (u usageJSON) usage() (gateway.Usage, bool) {
	us := gateway.Usage{
		Input: first(u.InputTokens, u.AInput), Output: first(u.OutputTokens, u.AOutput),
		CacheRead: first(u.CacheRead, u.ACacheRead), CacheWrite: first(u.CacheWrite, u.ACacheCreated),
	}
	return us, us.Input != gateway.Absent || us.Output != gateway.Absent || us.CacheRead != gateway.Absent || us.CacheWrite != gateway.Absent
}

func first(a, b *int) int {
	if a != nil {
		return *a
	}
	return val(b)
}

// zeroAbsent turns absent fields into zero, for a complete (non-stream) response.
func zeroAbsent(u gateway.Usage) gateway.Usage {
	for _, f := range []*int{&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite} {
		if *f < 0 {
			*f = 0
		}
	}
	return u
}

// usageFromResponse reads the usage of a complete Converse or InvokeModel response: the body's usage
// object (Converse: usage; Anthropic: usage), else the Bedrock token count headers.
func usageFromResponse(h http.Header, body []byte) (gateway.Usage, bool) {
	var doc struct {
		Usage usageJSON `json:"usage"`
	}
	if json.Unmarshal(body, &doc) == nil {
		if u, ok := doc.Usage.usage(); ok {
			return zeroAbsent(u), true
		}
	}
	in, inOK := headerInt(h, "X-Amzn-Bedrock-Input-Token-Count")
	out, outOK := headerInt(h, "X-Amzn-Bedrock-Output-Token-Count")
	if inOK || outOK {
		return gateway.Usage{Input: in, Output: out}, true
	}
	return gateway.Usage{}, false
}

func headerInt(h http.Header, k string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(h.Get(k), "%d", &n); err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
