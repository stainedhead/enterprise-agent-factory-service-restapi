package mcpgateway

import (
	"container/list"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

// metaToolDefs are the fixed definitions of gateway contract §13.3 (CD-47). Never change them within a
// major version: their bytes are part of every agent's prompt cache.
const metaToolDefs = `[
{"name":"afs_search","title":"Search tools","description":"Search the MCP tools, resources and prompts this agent may use. Returns names and one-line summaries, no schemas. Use afs_describe for schemas.","inputSchema":{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":500,"description":"What you want to do, in words."},"servers":{"type":"array","items":{"type":"string"},"maxItems":20,"description":"Limit to these server names."},"kinds":{"type":"array","items":{"type":"string","enum":["tool","resource","prompt"]},"description":"Limit to these kinds."},"limit":{"type":"integer","minimum":1,"maximum":25,"default":8}},"required":["query"]},"annotations":{"readOnlyHint":true,"openWorldHint":false}},
{"name":"afs_describe","title":"Describe tools","description":"Return the full definitions (input and output schemas, annotations, resource templates, prompt arguments) of up to 10 names found with afs_search.","inputSchema":{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":10}},"required":["names"]},"annotations":{"readOnlyHint":true,"openWorldHint":false}},
{"name":"afs_call","title":"Call a tool","description":"Call a tool, read a resource or get a prompt by its name from afs_search, with its arguments. Returns the server's result unchanged.","inputSchema":{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object"}},"required":["name"]}}
]`

var metaDefs = func() []json.RawMessage {
	var defs []json.RawMessage
	if err := json.Unmarshal([]byte(metaToolDefs), &defs); err != nil {
		panic(err)
	}
	for i, d := range defs {
		var v any
		_ = json.Unmarshal(d, &v)
		defs[i], _ = json.Marshal(v) // compact, stable key order
	}
	return defs
}()

func metaTools() []json.RawMessage { return append([]json.RawMessage(nil), metaDefs...) }

func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Primitive.QualifiedName() < es[j].Primitive.QualifiedName() })
}

// textResult wraps structured content as a tool result, with a text copy for clients that read only
// content.
func textResult(v any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}, "structuredContent": v}
}

func (c *call) search(args json.RawMessage) (int, *mcpwire.Message) {
	var a struct {
		Query   string   `json:"query"`
		Servers []string `json:"servers"`
		Kinds   []string `json:"kinds"`
		Limit   int      `json:"limit"`
	}
	if json.Unmarshal(args, &a) != nil || strings.TrimSpace(a.Query) == "" || len(a.Query) > 500 || len(a.Servers) > 20 {
		return c.protocolError(mcpwire.CodeInvalidParams, "afs_search needs a query of 1 to 500 characters")
	}
	kinds := make([]mcp.Kind, 0, len(a.Kinds))
	for _, k := range a.Kinds {
		switch mcp.Kind(k) {
		case mcp.KindTool, mcp.KindResource, mcp.KindPrompt:
			kinds = append(kinds, mcp.Kind(k))
		default:
			return c.protocolError(mcpwire.CodeInvalidParams, "kinds must be tool, resource or prompt")
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = mcp.DefaultSearchLimit
	}
	limit = min(limit, c.h.cfg.SearchLimitMax)
	ix := c.h.index.get(c.snap)
	q := mcp.Query{Text: a.Query, Servers: a.Servers, Kinds: kinds, Limit: limit}
	if c.h.cfg.Boost != nil {
		q.Boost = c.h.cfg.Boost(c.ctx, c.caller)
	}
	hits := ix.Search(q)
	type result struct {
		Name        string          `json:"name"`
		Server      string          `json:"server"`
		Kind        mcp.Kind        `json:"kind"`
		Title       string          `json:"title,omitempty"`
		Summary     string          `json:"summary,omitempty"`
		Annotations json.RawMessage `json:"annotations,omitempty"`
		Score       float64         `json:"score"`
	}
	out := make([]result, 0, len(hits))
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		p := h.Primitive
		r := result{Name: p.QualifiedName(), Server: p.Server, Kind: p.Kind, Title: p.Title, Summary: Summary(p.Description), Score: round3(h.Score)}
		if p.Kind == mcp.KindTool {
			r.Annotations, _ = json.Marshal(p.Annotations)
		}
		out = append(out, r)
		names = append(names, r.Name)
	}
	hit := len(out) > 0
	c.ev.SearchResults, c.ev.SearchHit, c.ev.QueryText = names, &hit, a.Query
	c.ev.Name = "afs_search"
	return c.ok(textResult(map[string]any{"results": out}))
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

// Summary is the first sentence of a description, at most 200 characters.
func Summary(d string) string {
	d = strings.TrimSpace(strings.ReplaceAll(d, "\n", " "))
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	if r := []rune(d); len(r) > 200 {
		d = string(r[:199]) + "…"
	}
	return d
}

func (c *call) describe(args json.RawMessage) (int, *mcpwire.Message) {
	var a struct {
		Names []string `json:"names"`
	}
	if json.Unmarshal(args, &a) != nil || len(a.Names) == 0 || len(a.Names) > 10 {
		return c.protocolError(mcpwire.CodeInvalidParams, "afs_describe needs 1 to 10 names")
	}
	defs := []json.RawMessage{}
	notFound := []string{}
	for _, n := range a.Names {
		var e *Entry
		for _, k := range []mcp.Kind{mcp.KindTool, mcp.KindResource, mcp.KindResourceTemplate, mcp.KindPrompt} {
			if x, _, ok := c.lookupExact(k, n); ok && x.Decision.Allowed {
				e = x
				break
			}
		}
		if e == nil {
			notFound = append(notFound, n)
			continue
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(c.present(*e), &obj)
		if obj == nil {
			obj = map[string]json.RawMessage{}
		}
		k, _ := json.Marshal(e.Primitive.Kind)
		obj["kind"] = k
		b, _ := json.Marshal(obj)
		defs = append(defs, b)
	}
	c.ev.Name = "afs_describe"
	return c.ok(textResult(map[string]any{"definitions": defs, "notFound": notFound}))
}

// lookupExact is lookup without template matching.
func (c *call) lookupExact(kind mcp.Kind, name string) (*Entry, string, bool) {
	s, n, ok := mcp.SplitQualifiedName(name)
	if !ok {
		return nil, "", false
	}
	for i := range c.snap.Entries {
		e := &c.snap.Entries[i]
		if e.Primitive.Server == s && e.Primitive.Kind == kind && e.Primitive.Name == n {
			return e, n, true
		}
	}
	return nil, n, false
}

// metaCall runs afs_call: the named primitive decides whether it is a tool call, a resource read or a
// prompt get.
func (c *call) metaCall(args json.RawMessage) (int, *mcpwire.Message) {
	var a struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(args, &a) != nil || a.Name == "" {
		return c.protocolError(mcpwire.CodeInvalidParams, "afs_call needs a name")
	}
	if a.Arguments == nil {
		a.Arguments = json.RawMessage("{}")
	}
	for _, k := range []mcp.Kind{mcp.KindTool, mcp.KindPrompt, mcp.KindResource} {
		e, _, ok := c.lookup(k, a.Name)
		if !ok {
			continue
		}
		_ = e
		switch k {
		case mcp.KindTool:
			p, _ := json.Marshal(map[string]json.RawMessage{"name": mustJSON(a.Name), "arguments": a.Arguments})
			return c.forward(k, a.Name, "name", p)
		case mcp.KindPrompt:
			p, _ := json.Marshal(map[string]json.RawMessage{"name": mustJSON(a.Name), "arguments": a.Arguments})
			c.msg.Method = "prompts/get"
			return c.forward(k, a.Name, "name", p)
		default:
			p, _ := json.Marshal(map[string]json.RawMessage{"uri": mustJSON(a.Name)})
			c.msg.Method = "resources/read"
			return c.forward(k, a.Name, "uri", p)
		}
	}
	c.refuse("primitive_unknown")
	c.ev.Name = a.Name
	return http.StatusOK, mcpwire.NewError(c.msg.ID, mcpwire.CodeInvalidParams, "Unknown tool: "+a.Name, nil)
}

func mustJSON(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

// indexCache keeps search indexes by snapshot version (LRU).
type indexCache struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type indexItem struct {
	key string
	ix  *mcp.Index
}

func newIndexCache(max int) *indexCache {
	return &indexCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (ic *indexCache) get(s *Snapshot) *mcp.Index {
	build := func() *mcp.Index {
		var ps []mcp.Primitive
		for _, e := range s.Entries {
			if e.Decision.Allowed {
				ps = append(ps, e.Primitive)
			}
		}
		return mcp.NewIndex(ps)
	}
	if s.Version == "" {
		return build()
	}
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if el, ok := ic.items[s.Version]; ok {
		ic.order.MoveToFront(el)
		return el.Value.(*indexItem).ix
	}
	ix := build()
	ic.items[s.Version] = ic.order.PushFront(&indexItem{key: s.Version, ix: ix})
	for ic.order.Len() > ic.max {
		old := ic.order.Back()
		ic.order.Remove(old)
		delete(ic.items, old.Value.(*indexItem).key)
	}
	return ix
}
