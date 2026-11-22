package httpapi

import (
	"fmt"
	"net/http"
	"sort"
)

// Registry binds handlers to operations. A stream's registrar calls Handle for each operation it
// implements; every other operation keeps its 501 placeholder.
type Registry struct {
	ops   map[string]*operation
	bound map[string]bool
}

// Handle binds h to operationID. Unknown and already bound operations are errors.
func (g *Registry) Handle(operationID string, h http.Handler) error {
	op, ok := g.ops[operationID]
	if !ok {
		return fmt.Errorf("httpapi: unknown operation %s", operationID)
	}
	if g.bound[operationID] {
		return fmt.Errorf("httpapi: operation %s bound twice", operationID)
	}
	g.bound[operationID] = true
	op.handler = h
	return nil
}

// Bound lists the bound operations, sorted.
func (g *Registry) Bound() []string {
	out := make([]string, 0, len(g.bound))
	for id := range g.bound {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Unbound lists the operations without a handler, sorted.
func (g *Registry) Unbound() []string {
	var out []string
	for id := range g.ops {
		if !g.bound[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Registrar binds the handlers of one area.
type Registrar func(*Registry) error
