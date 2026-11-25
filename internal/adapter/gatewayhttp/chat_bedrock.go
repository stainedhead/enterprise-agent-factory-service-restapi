package gatewayhttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// The bedrock route (gateway contract §12.2): POST /r/bedrock/v1/chat/completions takes an OpenAI Chat
// Completions body whose model is an AFS model id, translates it to Bedrock Converse and the answer back.
// Admission, reservation, tags and usage are the Bedrock path's own (§2.1, §4). A field the translation
// cannot map is refused with 400 unsupported_parameter rather than silently dropped.

// chatKnown are the Chat Completions fields the translation maps or may ignore.
var chatKnown = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true, "temperature": true,
	"top_p": true, "stop": true, "stream": true, "stream_options": true, "tools": true, "tool_choice": true,
	"user": true, "n": true, "logprobs": true, "parallel_tool_calls": true,
}

func unsupported(field string) error {
	return &domain.Error{Kind: domain.KindValidation, Message: "the bedrock route cannot map " + field,
		Fields:  []domain.FieldError{{Field: field, Reason: domain.ReasonUnsupportedParameter}},
		Details: map[string]any{"reason": domain.ReasonUnsupportedParameter, "field": field}}
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// chatText returns the text of a message content: a string or an array of text parts.
func chatText(raw json.RawMessage, field string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", badRequest(field, "must be a string or an array of content parts")
	}
	var b strings.Builder
	for i, p := range parts {
		if p.Type != "text" {
			return "", unsupported(fmt.Sprintf("%s[%d].type", field, i))
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

type cvBlock map[string]any

// chatToConverse translates a Chat Completions body to a Converse body. It returns the AFS model id, the
// request's own output limit and whether the call streams.
func chatToConverse(body []byte) (conv []byte, modelID string, maxTokens int, stream, includeUsage bool, err error) {
	doc, err := parseDoc(body)
	if err != nil {
		return nil, "", 0, false, false, err
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !chatKnown[k] {
			return nil, "", 0, false, false, unsupported(k)
		}
	}
	if raw, ok := doc["n"]; ok && string(raw) != "1" && string(raw) != "null" {
		return nil, "", 0, false, false, unsupported("n")
	}
	if raw, ok := doc["logprobs"]; ok && string(raw) != "false" && string(raw) != "null" {
		return nil, "", 0, false, false, unsupported("logprobs")
	}
	if json.Unmarshal(doc["model"], &modelID) != nil || modelID == "" {
		return nil, "", 0, false, false, badRequest("model", "is required")
	}
	if raw, ok := doc["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	if raw, ok := doc["stream_options"]; ok {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		_ = json.Unmarshal(raw, &so)
		includeUsage = so.IncludeUsage
	}
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if n, ok := pathInt(doc, k); ok && maxTokens == 0 {
			maxTokens = n
		}
	}
	var msgs []chatMessage
	if err := json.Unmarshal(doc["messages"], &msgs); err != nil || len(msgs) == 0 {
		return nil, "", 0, false, false, badRequest("messages", "must be a non-empty array")
	}
	out := map[string]any{}
	var system []cvBlock
	var messages []map[string]any
	add := func(role string, blocks ...cvBlock) {
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			messages[n-1]["content"] = append(messages[n-1]["content"].([]cvBlock), blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	for i, m := range msgs {
		f := fmt.Sprintf("messages[%d]", i)
		text, err := chatText(m.Content, f+".content")
		if err != nil {
			return nil, "", 0, false, false, err
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, cvBlock{"text": text})
		case "user":
			add("user", cvBlock{"text": text})
		case "assistant":
			var blocks []cvBlock
			if text != "" {
				blocks = append(blocks, cvBlock{"text": text})
			}
			for j, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if tc.Function.Arguments != "" && json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
					return nil, "", 0, false, false, badRequest(fmt.Sprintf("%s.tool_calls[%d].function.arguments", f, j), "must be JSON")
				}
				blocks = append(blocks, cvBlock{"toolUse": map[string]any{"toolUseId": tc.ID, "name": tc.Function.Name, "input": input}})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, cvBlock{"text": ""})
			}
			add("assistant", blocks...)
		case "tool":
			add("user", cvBlock{"toolResult": map[string]any{"toolUseId": m.ToolCallID, "content": []cvBlock{{"text": text}}}})
		default:
			return nil, "", 0, false, false, unsupported(f + ".role")
		}
	}
	out["messages"] = messages
	if len(system) > 0 {
		out["system"] = system
	}
	ic := map[string]any{}
	for _, k := range []string{"temperature", "top_p"} {
		if raw, ok := doc[k]; ok && string(raw) != "null" {
			var v float64
			if json.Unmarshal(raw, &v) != nil {
				return nil, "", 0, false, false, badRequest(k, "must be a number")
			}
			ic[map[string]string{"temperature": "temperature", "top_p": "topP"}[k]] = v
		}
	}
	if raw, ok := doc["stop"]; ok && string(raw) != "null" {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			ic["stopSequences"] = []string{one}
		case json.Unmarshal(raw, &many) == nil:
			ic["stopSequences"] = many
		default:
			return nil, "", 0, false, false, badRequest("stop", "must be a string or an array of strings")
		}
	}
	if maxTokens > 0 {
		ic["maxTokens"] = maxTokens
	}
	if len(ic) > 0 {
		out["inferenceConfig"] = ic
	}
	if raw, ok := doc["tools"]; ok && string(raw) != "null" {
		var tools []chatTool
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, "", 0, false, false, badRequest("tools", "must be an array")
		}
		var specs []cvBlock
		for i, t := range tools {
			if t.Type != "function" {
				return nil, "", 0, false, false, unsupported(fmt.Sprintf("tools[%d].type", i))
			}
			params := t.Function.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			spec := map[string]any{"name": t.Function.Name, "inputSchema": map[string]any{"json": params}}
			if t.Function.Description != "" {
				spec["description"] = t.Function.Description
			}
			specs = append(specs, cvBlock{"toolSpec": spec})
		}
		tc := map[string]any{"tools": specs}
		if raw, ok := doc["tool_choice"]; ok && string(raw) != "null" {
			choice, err := toolChoice(raw)
			if err != nil {
				return nil, "", 0, false, false, err
			}
			if choice != nil {
				tc["toolChoice"] = choice
			}
		}
		out["toolConfig"] = tc
	}
	conv, err = json.Marshal(out)
	if err != nil {
		return nil, "", 0, false, false, domain.Internal(err)
	}
	return conv, modelID, maxTokens, stream, includeUsage, nil
}

func toolChoice(raw json.RawMessage) (map[string]any, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"auto": map[string]any{}}, nil
		case "required":
			return map[string]any{"any": map[string]any{}}, nil
		}
		return nil, unsupported("tool_choice")
	}
	var o struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Function.Name == "" {
		return nil, unsupported("tool_choice")
	}
	return map[string]any{"tool": map[string]any{"name": o.Function.Name}}, nil
}

// parseChatBedrock reads a /r/bedrock chat body as a Converse call the Bedrock pipeline can run.
func parseChatBedrock(body []byte) (*parsed, error) {
	conv, modelID, _, stream, includeUsage, err := chatToConverse(body)
	if err != nil {
		return nil, err
	}
	op, rtOp := gateway.OpConverse, bedrock.OpConverse
	if stream {
		op, rtOp = gateway.OpConverseStream, bedrock.OpConverseStream
	}
	p, err := parseConverse(rtOp, op, modelID, conv)
	if err != nil {
		return nil, err
	}
	p.wire, p.stream, p.includeUsage = wireChat, stream, includeUsage
	return p, nil
}

var finishReasons = map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls",
	"content_filtered": "content_filter", "guardrail_intervened": "content_filter"}

func finishReason(stop string) string {
	if r, ok := finishReasons[stop]; ok {
		return r
	}
	return "stop"
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	Details          *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func toChatUsage(u gateway.Usage) chatUsage {
	cu := chatUsage{PromptTokens: u.Input + u.CacheRead + u.CacheWrite, CompletionTokens: u.Output}
	cu.TotalTokens = cu.PromptTokens + cu.CompletionTokens
	if u.CacheRead > 0 {
		cu.Details = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{u.CacheRead}
	}
	return cu
}

// converseToChat translates a complete Converse answer to a chat.completion object.
func converseToChat(body []byte, modelID, requestID string, created time.Time) ([]byte, error) {
	var r struct {
		Output struct {
			Message struct {
				Content []struct {
					Text    *string `json:"text"`
					ToolUse *struct {
						ToolUseID string          `json:"toolUseId"`
						Name      string          `json:"name"`
						Input     json.RawMessage `json:"input"`
					} `json:"toolUse"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string    `json:"stopReason"`
		Usage      usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &domain.Error{Kind: domain.KindUpstream, Message: "the model returned an unreadable answer", Cause: err}
	}
	var text strings.Builder
	calls := []map[string]any{}
	for _, c := range r.Output.Message.Content {
		if c.Text != nil {
			text.WriteString(*c.Text)
		}
		if c.ToolUse != nil {
			calls = append(calls, map[string]any{"id": c.ToolUse.ToolUseID, "type": "function",
				"function": map[string]any{"name": c.ToolUse.Name, "arguments": string(c.ToolUse.Input)}})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	u, _ := r.Usage.usage()
	return json.Marshal(map[string]any{"id": "chatcmpl-" + requestID, "object": "chat.completion", "created": created.Unix(), "model": modelID,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason(r.StopReason)}}, "usage": toChatUsage(u)})
}

// chatChunkWriter turns ConverseStream events into chat.completion.chunk server-sent events.
type chatChunkWriter struct {
	rw           *respWriter
	model        string
	id           string
	created      int64
	includeUsage bool
	tools        map[int]int // content block index -> tool call index
}

func (w *chatChunkWriter) chunk(delta map[string]any, finish *string, usage *chatUsage) error {
	choices := []map[string]any{}
	if delta != nil || finish != nil {
		c := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
		if delta == nil {
			c["delta"] = map[string]any{}
		}
		choices = append(choices, c)
	}
	m := map[string]any{"id": w.id, "object": "chat.completion.chunk", "created": w.created, "model": w.model, "choices": choices}
	if usage != nil {
		m["usage"] = usage
	}
	b, _ := json.Marshal(m)
	return w.data(b)
}

func (w *chatChunkWriter) data(b []byte) error {
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	if _, err := w.rw.Write(buf.Bytes()); err != nil {
		return errClientWrite
	}
	w.rw.flush()
	return nil
}

func (w *chatChunkWriter) event(m eventstream.Message) error {
	var d struct {
		Role              string `json:"role"`
		ContentBlockIndex int    `json:"contentBlockIndex"`
		Start             struct {
			ToolUse *struct {
				ToolUseID string `json:"toolUseId"`
				Name      string `json:"name"`
			} `json:"toolUse"`
		} `json:"start"`
		Delta struct {
			Text    *string `json:"text"`
			ToolUse *struct {
				Input string `json:"input"`
			} `json:"toolUse"`
		} `json:"delta"`
		StopReason string    `json:"stopReason"`
		Usage      usageJSON `json:"usage"`
	}
	_ = json.Unmarshal(m.Payload, &d)
	switch m.Headers.Get(":event-type").String() {
	case "messageStart":
		return w.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	case "contentBlockStart":
		if d.Start.ToolUse != nil {
			if w.tools == nil {
				w.tools = map[int]int{}
			}
			i := len(w.tools)
			w.tools[d.ContentBlockIndex] = i
			return w.chunk(map[string]any{"tool_calls": []map[string]any{{"index": i, "id": d.Start.ToolUse.ToolUseID, "type": "function",
				"function": map[string]any{"name": d.Start.ToolUse.Name, "arguments": ""}}}}, nil, nil)
		}
	case "contentBlockDelta":
		if d.Delta.Text != nil {
			return w.chunk(map[string]any{"content": *d.Delta.Text}, nil, nil)
		}
		if d.Delta.ToolUse != nil {
			return w.chunk(map[string]any{"tool_calls": []map[string]any{{"index": w.tools[d.ContentBlockIndex],
				"function": map[string]any{"arguments": d.Delta.ToolUse.Input}}}}, nil, nil)
		}
	case "messageStop":
		f := finishReason(d.StopReason)
		return w.chunk(nil, &f, nil)
	case "metadata":
		if u, ok := d.Usage.usage(); ok && w.includeUsage {
			cu := toChatUsage(u)
			if err := w.chunk(nil, nil, &cu); err != nil {
				return err
			}
		}
		return w.data([]byte("[DONE]"))
	}
	return nil
}

func (w *chatChunkWriter) fail(err error, requestID string) error {
	_, p := toProblem(err, requestID, false)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": p.Message, "type": p.Code, "code": p.Code}})
	return w.data(b)
}

// writeChatComplete sends a translated Converse answer.
func writeChatComplete(rw *respWriter, out *bedrock.Response, modelID string) {
	b, err := converseToChat(out.Body, modelID, rw.requestID, time.Now())
	if err != nil {
		rw.writeProblem(err, 0, false)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Content-Length", strconv.Itoa(len(b)))
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(b)
}
