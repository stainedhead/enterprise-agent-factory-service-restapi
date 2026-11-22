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

// Registrar binds the handlers of one area.
type Registrar func(*Registry) error

// Area is one handler area of architecture.md §3; its stream owns its registrar.
type Area struct {
	Name     string
	Register Registrar
}

func none(*Registry) error { return nil }

// Areas lists the handler areas. Each stream replaces its area's registrar with one in its own
// file (for example teams.go) when it binds its first operation.
var Areas = []Area{
	{"teams", none}, {"members", none}, {"apikeys", none}, {"me", none}, {"agents", none},
	{"pools", none}, {"identity", none}, {"identityproviders", none}, {"secrets", none}, {"config", none}, {"extsys", none},
	{"budgets", none}, {"reports", none}, {"delivery", none}, {"audit", none}, {"revisions", none},
	{"workorders", none}, {"swarmtemplates", none}, {"admin", none}, {"adminmodels", none},
	{"adminsettings", none}, {"exports", none},
}
