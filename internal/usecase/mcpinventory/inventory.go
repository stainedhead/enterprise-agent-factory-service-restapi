// Package mcpinventory refreshes a server's inventory of primitives (MCP PRD §6.3, §7.4, §8; gateway
// contract §13.6). It lists the upstream, pins each definition by digest, holds new and changed
// definitions for review unless the server's drift policy allows an automatic approval, runs the
// definition checks, and returns a plan the store applies in one transaction.
package mcpinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

// Lists are the MCP list methods and the result field each returns.
var Lists = []struct {
	Method string
	Field  string
	Kind   mcp.Kind
}{
	{"tools/list", "tools", mcp.KindTool},
	{"resources/list", "resources", mcp.KindResource},
	{"resources/templates/list", "resourceTemplates", mcp.KindResourceTemplate},
	{"prompts/list", "prompts", mcp.KindPrompt},
}

// MaxPrimitives is the inventory cap per server (database contract §7.5.1).
const MaxPrimitives = 2000

// maxPages stops a server that never ends its pagination.
const maxPages = 200

// Lister calls one list method on the upstream. A server that does not offer a capability returns
// ErrUnsupported for its methods.
type Lister interface {
	List(ctx context.Context, method, cursor string) (items []json.RawMessage, next string, err error)
}

// ErrUnsupported means the server does not offer the method (JSON-RPC -32601).
var ErrUnsupported = errors.New("mcpinventory: method not supported by the server")

// Stored is one primitive as the database holds it.
type Stored struct {
	ID             string
	Kind           mcp.Kind
	Name           string
	Definition     json.RawMessage // approved definition (nil if never approved)
	Digest         string          // digest of the newest seen definition
	ApprovedDigest string
	Status         mcp.Status
	Annotations    *mcp.Annotations // approved annotations
	Removed        bool
}

// Server is what the refresh needs to know about the server.
type Server struct {
	ID           string
	Slug         string
	DriftPolicy  string   // hold or auto_approve_compatible
	OtherServers []string // slugs the same teams can reach, for the cross-server check
}

// Action is what happens to one primitive.
type Action string

// Actions.
const (
	ActionUnchanged   Action = "unchanged"    // last_seen only
	ActionInsertHeld  Action = "insert_held"  // new, pending_review
	ActionHold        Action = "hold"         // changed, pending_review with pending_definition; the approved definition stays served
	ActionAutoApprove Action = "auto_approve" // changed compatibly under auto_approve_compatible
	ActionRemove      Action = "remove"       // no longer offered
	ActionReappear    Action = "reappear"     // offered again with the approved digest
)

// Item is one planned change.
type Item struct {
	Action     Action
	StoredID   string // empty for inserts
	Primitive  mcp.Primitive
	Definition json.RawMessage
	Digest     string
	Change     mcp.Change
}

// Plan is the result of a refresh.
type Plan struct {
	Items    []Item
	Findings []mcp.Finding
	// Events are the primitive_changed events to emit: held changes and automatic approvals.
	Events []Item
	// Truncated is set when the server offered more than MaxPrimitives; the rest are ignored.
	Truncated bool
}

// Fetch lists every supported list method, following cursors.
func Fetch(ctx context.Context, l Lister) (map[mcp.Kind][]json.RawMessage, bool, error) {
	out := map[mcp.Kind][]json.RawMessage{}
	total, truncated := 0, false
	for _, ls := range Lists {
		cursor := ""
		for page := 0; ; page++ {
			if page == maxPages {
				return nil, false, fmt.Errorf("mcpinventory: %s did not end after %d pages", ls.Method, maxPages)
			}
			items, next, err := l.List(ctx, ls.Method, cursor)
			if errors.Is(err, ErrUnsupported) {
				break
			}
			if err != nil {
				return nil, false, fmt.Errorf("mcpinventory: %s: %w", ls.Method, err)
			}
			for _, it := range items {
				if total == MaxPrimitives {
					truncated = true
					break
				}
				out[ls.Kind] = append(out[ls.Kind], it)
				total++
			}
			if next == "" || truncated {
				break
			}
			cursor = next
		}
	}
	return out, truncated, nil
}

// Describe turns a definition object into a Primitive. It returns false when the object has no name.
func Describe(server string, kind mcp.Kind, def json.RawMessage) (mcp.Primitive, bool) {
	var d struct {
		Name        string          `json:"name"`
		URI         string          `json:"uri"`
		URITemplate string          `json:"uriTemplate"`
		Title       string          `json:"title"`
		Description string          `json:"description"`
		Annotations mcp.Annotations `json:"annotations"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"inputSchema"`
		Arguments []struct {
			Name string `json:"name"`
		} `json:"arguments"`
	}
	if json.Unmarshal(def, &d) != nil {
		return mcp.Primitive{}, false
	}
	p := mcp.Primitive{Server: server, Kind: kind, Name: d.Name, Title: d.Title, Description: d.Description}
	switch kind {
	case mcp.KindResource:
		p.Name = d.URI
		if p.Title == "" {
			p.Title = d.Name
		}
	case mcp.KindResourceTemplate:
		p.Name = d.URITemplate
		if p.Title == "" {
			p.Title = d.Name
		}
	case mcp.KindTool:
		p.Annotations = d.Annotations
		for k := range d.InputSchema.Properties {
			p.Params = append(p.Params, k)
		}
		sort.Strings(p.Params)
	case mcp.KindPrompt:
		for _, a := range d.Arguments {
			p.Params = append(p.Params, a.Name)
		}
	}
	return p, p.Name != ""
}

type key struct {
	kind mcp.Kind
	name string
}

// Build compares a fresh listing with the stored inventory and plans the changes. It never approves a
// new primitive, an incompatible change or one that crosses a risk line; it auto-approves a compatible
// change only under auto_approve_compatible.
func Build(s Server, stored []Stored, listed map[mcp.Kind][]json.RawMessage, truncated bool) (Plan, error) {
	plan := Plan{Truncated: truncated}
	byKey := make(map[key]*Stored, len(stored))
	for i := range stored {
		byKey[key{stored[i].Kind, stored[i].Name}] = &stored[i]
	}
	seen := map[key]bool{}
	var prims []mcp.Primitive
	for _, ls := range Lists {
		for _, def := range listed[ls.Kind] {
			p, ok := Describe(s.Slug, ls.Kind, def)
			if !ok {
				continue
			}
			k := key{p.Kind, p.Name}
			prims = append(prims, p)
			if seen[k] {
				continue // the duplicate is reported by the checks
			}
			seen[k] = true
			digest, err := mcp.Digest(def)
			if err != nil {
				return Plan{}, fmt.Errorf("mcpinventory: %s %s: %w", p.Kind, p.Name, err)
			}
			canon, _ := mcp.Canonicalize(def)
			it := Item{Primitive: p, Definition: canon, Digest: digest}
			st := byKey[k]
			if st == nil {
				it.Action, it.Change = ActionInsertHeld, mcp.ChangeNew
				plan.Events = append(plan.Events, it)
			} else {
				it.StoredID = st.ID
				it.Change = mcp.Compare(st.Definition, canon, st.ApprovedDigest, digest)
				switch {
				case st.Removed && digest == st.ApprovedDigest:
					it.Action = ActionReappear
				case it.Change == mcp.ChangeNone && digest == st.Digest:
					it.Action = ActionUnchanged
				case it.Change == mcp.ChangeNone:
					it.Action = ActionUnchanged // back to the approved definition; the store clears the hold
				case st.Digest == digest && st.Status == mcp.StatusPendingReview:
					it.Action = ActionUnchanged // already held
				case it.Change == mcp.ChangeCompatible && s.DriftPolicy == "auto_approve_compatible" &&
					st.Status == mcp.StatusApproved && !mcp.CrossesRiskLine(st.Annotations, p.Annotations, p.Kind):
					it.Action = ActionAutoApprove
					plan.Events = append(plan.Events, it)
				default:
					it.Action = ActionHold
					plan.Events = append(plan.Events, it)
				}
			}
			plan.Items = append(plan.Items, it)
		}
	}
	for i := range stored {
		st := &stored[i]
		if !st.Removed && !seen[key{st.Kind, st.Name}] && !truncated {
			plan.Items = append(plan.Items, Item{Action: ActionRemove, StoredID: st.ID,
				Primitive: mcp.Primitive{Server: s.Slug, Kind: st.Kind, Name: st.Name}})
		}
	}
	plan.Findings = mcp.CheckDefinitions(prims, s.OtherServers)
	// A server whose definitions fail a check gets nothing auto-approved.
	if len(plan.Findings) > 0 {
		for i := range plan.Items {
			if plan.Items[i].Action == ActionAutoApprove {
				plan.Items[i].Action = ActionHold
			}
		}
		for i := range plan.Events {
			if plan.Events[i].Action == ActionAutoApprove {
				plan.Events[i].Action = ActionHold
			}
		}
	}
	return plan, nil
}
