package domain

import (
	"fmt"
	"reflect"
	"sort"
)

// RestoreAction is what a restore does to one resource.
type RestoreAction string

// Restore actions.
const (
	RestoreCreate RestoreAction = "create"
	RestoreUpdate RestoreAction = "update"
	RestoreDelete RestoreAction = "delete"
	RestoreSkip   RestoreAction = "skip"
)

// FieldChange is one field of an update.
type FieldChange struct {
	Field string
	From  any
	To    any
}

// RestoreChange is one line of a restore plan or result (API contract §4.10, RestoreChange).
type RestoreChange struct {
	ResourceKind string
	ResourceID   string
	Action       RestoreAction
	Fields       []FieldChange
	// Reason says why a resource was skipped; empty otherwise.
	Reason string
}

// RestoreStep is a planned change with the row it restores to (nil for a delete).
type RestoreStep struct {
	Change RestoreChange
	Row    Row
}

// RestoreSpec says how one kind of resource is restored: the fields put back, and whether a resource that
// exists now but did not then may be deleted (Delete) and one that existed then but not now re-created
// (Create). Resources that cannot safely come back or go away through a restore (agents, the team itself)
// are updated in place and otherwise skipped with a reason.
type RestoreSpec struct {
	Kind   string
	Fields []string
	Create bool
	Update bool
	Delete bool
	// Note explains a limit of the kind in the plan documentation.
	Note string
}

// RestoreSpecs lists what a restore covers, in apply order (RA-13): team settings, agents, configuration
// values, external system switches, tags and budget. Each kind is the table whose revisions are read.
//
// Agent secret bindings (references and a target name, never a value) are restored since -db 000018 gave the
// table a revision trigger; a binding whose agent or secret is gone is skipped. Role bindings (membership)
// are never restored, so a restore cannot lock a team out.
var RestoreSpecs = []RestoreSpec{
	{Kind: "teams", Update: true, Fields: []string{"display_name", "description", "owner_email", "cost_center"},
		Note: "team settings are updated in place"},
	{Kind: "agents", Update: true, Fields: []string{"description", "persona", "template", "image", "model_id", "resources", "state",
		"snapshot_schedule", "snapshot_retain_count", "storage_on_delete"},
		Note: "agents are updated in place; one created or deleted since the revision is left alone"},
	{Kind: "namespace_config_values", Create: true, Update: true, Delete: true, Fields: []string{"config_value", "description"}},
	{Kind: "agent_config_values", Create: true, Update: true, Delete: true, Fields: []string{"source", "config_value"}},
	{Kind: "external_system_optouts", Create: true, Delete: true, Fields: []string{"reason"}},
	{Kind: "agent_secret_bindings", Create: true, Update: true, Delete: true, Fields: []string{"secret_id", "delivery"},
		Note: "resource id is <team>/<agent>/<target>; a binding to an agent or secret that no longer exists is skipped"},
	{Kind: "team_tags", Create: true, Update: true, Delete: true, Fields: []string{"value"}},
	{Kind: "budgets", Create: true, Update: true, Fields: []string{"period", "amount_usd", "thresholds", "on_exhaust", "starts_at", "expires_at"}},
}

// RestoreKinds returns the kinds of RestoreSpecs.
func RestoreKinds() []string {
	out := make([]string, len(RestoreSpecs))
	for i, s := range RestoreSpecs {
		out[i] = s.Kind
	}
	return out
}

// RestoreState holds rows by kind and resource id. A resource absent from Target did not exist at the
// revision; one absent from Current does not exist now.
type RestoreState map[string]map[string]Row

// agent states a restore may put back; a deleting or deleted agent is never brought back.
var restorableAgentState = map[string]bool{"enabled": true, "paused": true}

// PlanRestore compares the team's state at a revision (target) with its state now (current) and returns
// the steps that make the team match the revision. Creates and updates come in RestoreSpecs order, then
// deletes in reverse order, so a value is never removed while something still links to it. Skips carry a
// reason. The plan is deterministic.
func PlanRestore(target, current RestoreState) []RestoreStep {
	var steps, deletes []RestoreStep
	for _, spec := range RestoreSpecs {
		ids := map[string]bool{}
		for id := range target[spec.Kind] {
			ids[id] = true
		}
		for id := range current[spec.Kind] {
			ids[id] = true
		}
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		for _, id := range sorted {
			t, c := target[spec.Kind][id], current[spec.Kind][id]
			ch := RestoreChange{ResourceKind: spec.Kind, ResourceID: id}
			switch {
			case t != nil && c != nil:
				if spec.Kind == "agents" && !restorableAgentState[str(t, "state")] {
					ch.Action, ch.Reason = RestoreSkip, "the agent was being deleted at that revision"
					steps = append(steps, RestoreStep{Change: ch})
					continue
				}
				if !spec.Update {
					continue
				}
				fields := diffFields(spec.Fields, t, c)
				if len(fields) == 0 {
					continue
				}
				ch.Action, ch.Fields = RestoreUpdate, fields
				steps = append(steps, RestoreStep{Change: ch, Row: t})
			case t != nil && c == nil:
				if !spec.Create {
					ch.Action, ch.Reason = RestoreSkip, fmt.Sprintf("%s that were deleted since the revision are not brought back", spec.Kind)
					if spec.Kind == "agents" {
						ch.Reason = "the agent was deleted since the revision; deleted agents are not brought back"
					}
					steps = append(steps, RestoreStep{Change: ch})
					continue
				}
				ch.Action = RestoreCreate
				steps = append(steps, RestoreStep{Change: ch, Row: t})
			case t == nil && c != nil:
				if !spec.Delete {
					if spec.Kind == "agents" {
						ch.Action, ch.Reason = RestoreSkip, "the agent was created after the revision; delete it explicitly if it is not wanted"
						steps = append(steps, RestoreStep{Change: ch})
					}
					continue
				}
				ch.Action = RestoreDelete
				deletes = append([]RestoreStep{{Change: ch}}, deletes...)
			}
		}
	}
	return append(steps, deletes...)
}

func str(r Row, k string) string { s, _ := r[k].(string); return s }

func diffFields(fields []string, t, c Row) []FieldChange {
	var out []FieldChange
	for _, f := range fields {
		tv, cv := t[f], c[f]
		if !sameJSON(tv, cv) {
			out = append(out, FieldChange{Field: f, From: cv, To: tv})
		}
	}
	return out
}

// sameJSON compares two decoded JSON values by their canonical encoding, so 400 and 400.00 written the
// same way by the database compare equal and object member order does not matter.
func sameJSON(a, b any) bool {
	ca, errA := CanonicalJSON(a)
	cb, errB := CanonicalJSON(b)
	if errA != nil || errB != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(ca) == string(cb)
}
