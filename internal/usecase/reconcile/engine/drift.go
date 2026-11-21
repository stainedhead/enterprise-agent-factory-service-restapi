package engine

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ObjectName is the name controllers and drift watchers use for an owned object, so a reconcile can
// tell which flagged objects it corrected: "Kind/namespace/name", or "Kind/name" when cluster scoped.
func ObjectName(kind, namespace, name string) string {
	if namespace == "" {
		return kind + "/" + name
	}
	return kind + "/" + namespace + "/" + name
}

// DriftRecord is one corrected drift, to be audited and announced.
type DriftRecord struct {
	Ref
	Object string
	At     time.Time
}

// DriftRecorder writes the drift.corrected audit record and outbox event (RC-US-3.1).
type DriftRecorder interface {
	Record(ctx context.Context, d DriftRecord) error
}

// NoteDrift is called by a drift watch when an owned object was edited or deleted by someone else. It
// queues the owner for an immediate reconcile; if that pass has to change the object, the correction
// is recorded once.
func (e *Engine) NoteDrift(ref Ref, object string) {
	e.mu.Lock()
	m := e.drift[ref.Key]
	if m == nil {
		m = map[string]time.Time{}
		e.drift[ref.Key] = m
	}
	if _, ok := m[object]; !ok {
		m[object] = e.cfg.Now()
	}
	e.mu.Unlock()
	e.queue.Add(ref)
}

func (e *Engine) takeDrift(key Key) map[string]time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.drift[key]
	delete(e.drift, key)
	return m
}

// correctedDrift returns the flagged objects the pass changed, sorted.
func correctedDrift(noted map[string]time.Time, changed []string) []string {
	var out []string
	for obj := range noted {
		if slices.Contains(changed, obj) {
			out = append(out, obj)
		}
	}
	sort.Strings(out)
	return out
}

func driftCondition(objects []string, at time.Time) domain.Condition {
	return domain.Condition{
		Type: domain.CondDriftCorrected, Status: domain.ConditionTrue, Reason: "ObjectReverted",
		Message: "reverted: " + strings.Join(objects, ", "), LastTransitionTime: at,
	}
}
