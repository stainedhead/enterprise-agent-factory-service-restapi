// Package tags computes and applies the effective tag set of the AWS resources the reconciler manages
// (RC-9, TAG-1, TAG-3, TAG-4, TAG-5; M4-B1). A resource carries the system tags AFS sets (afs-team-id,
// afs-team-slug and, for an agent, afs-agent-id and afs-agent-name), the service tags (fixed and required)
// and the team's own tags. The set is capped at 40 keys, which leaves room under AWS's 50 per resource for
// the tags the EBS CSI driver and other tooling add (D27, OQ-2). A set above the cap is refused before any
// AWS call, and the resource reports it in its TagsApplied condition.
//
// A change is applied by the next reconcile of the resource: tag and service tag changes notify the
// reconciler (and a 5-minute resync backs that up), well inside the 10 minutes of RC-US-7.1. Adapters
// remove a tag that was applied earlier and is no longer in the set with Stale: the keys AFS applied are
// recorded in the afs-tag-keys tag, because a tag key of the team's own carries no prefix to recognize it by.
package tags

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// System tag keys (TAG-1).
const (
	KeyTeamID    = "afs-team-id"
	KeyTeamSlug  = "afs-team-slug"
	KeyAgentID   = "afs-agent-id"
	KeyAgentName = "afs-agent-name"
	KeyPoolID    = "afs-pool-id"
	KeyPoolName  = "afs-pool-name"
	// KeyManagedBy marks the resources the reconciler created; it is owned by the adapters, not part of the set.
	KeyManagedBy = "afs-managed-by"
	// KeyOrphan is the sweep marker of an orphan secret; never touched here.
	KeyOrphan = "afs-orphan"
	// KeyApplied records the non-system keys AFS applied, comma separated and sorted.
	KeyApplied = "afs-tag-keys"
)

// Condition is the condition type that reports the tag set of a resource.
const Condition = "TagsApplied"

// ErrorCode is the code of a refused tag set, recorded as the condition's reason.
const ErrorCode = "TagLimitExceeded"

// MaxKeys is the cap of the effective set, system tags included (D27).
const MaxKeys = domain.MaxEffectiveTags

// Subject names who the tags are for.
type Subject struct {
	TeamID, TeamSlug string
	// AgentID and AgentName are set for an agent's resources.
	AgentID, AgentName string
	// PoolID and PoolName are set for a pool's resources.
	PoolID, PoolName string
}

// Set is a tag set: key to value.
type Set map[string]string

// LimitError says the effective set is above the cap.
type LimitError struct {
	Keys int
	Max  int
	// Subject is the team or agent id the set was built for.
	Subject string
}

// Error implements error. It names the limit.
func (e *LimitError) Error() string {
	return fmt.Sprintf("the effective tag set of %s has %d keys; the limit is %d (system, service and team tags together); remove team tags to apply it", e.Subject, e.Keys, e.Max)
}

// ErrorCode returns ErrorCode.
func (*LimitError) ErrorCode() string { return ErrorCode }

// Effective builds the set for subject from the tags the database holds (service fixed and required tags
// and the team's own, as afs.team_effective_tags lists them). System tags are always added and cannot be
// changed or removed by the input: a key with an afs- or aws: prefix in inherited is ignored. It returns a
// *LimitError, and no set, when the result has more than MaxKeys keys.
func Effective(s Subject, inherited map[string]string) (Set, error) {
	out := Set{KeyTeamID: s.TeamID}
	add := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	add(KeyTeamSlug, s.TeamSlug)
	add(KeyAgentID, s.AgentID)
	add(KeyAgentName, s.AgentName)
	add(KeyPoolID, s.PoolID)
	add(KeyPoolName, s.PoolName)
	for k, v := range inherited {
		if reserved(k) || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) > MaxKeys {
		id := s.AgentID
		if id == "" {
			id = s.PoolID
		}
		if id == "" {
			id = s.TeamID
		}
		return nil, &LimitError{Keys: len(out), Max: MaxKeys, Subject: id}
	}
	return out, nil
}

// reserved reports whether the key belongs to AFS or AWS.
func reserved(k string) bool {
	l := strings.ToLower(k)
	return strings.HasPrefix(l, "afs-") || strings.HasPrefix(l, "afs:") || strings.HasPrefix(l, "aws:")
}

// WithApplied returns the set to put on a resource: s plus the afs-tag-keys record of its non-system
// keys. The record is left off when it would not fit a tag value (256 characters); removal then falls
// back to the afs- prefix rule alone, which is the case for the system keys.
func (s Set) WithApplied() Set {
	out := make(Set, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	if rec := s.appliedRecord(); rec != "" && len(rec) <= 256 {
		out[KeyApplied] = rec
	}
	return out
}

func (s Set) appliedRecord() string {
	var keys []string
	for k := range s {
		if !reserved(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// Stale returns the keys to remove from a resource whose tags are have, when the set to apply is want
// (already WithApplied): the keys recorded in have's afs-tag-keys that are not in want, and the afs- keys
// AFS sets that are not in want. Tags put on the resource by anyone else are never listed, and neither are
// afs-managed-by and afs-orphan.
func Stale(have, want map[string]string) []string {
	var out []string
	for _, k := range strings.Split(have[KeyApplied], ",") {
		if _, ok := want[k]; k != "" && !ok && !reserved(k) {
			if _, present := have[k]; present {
				out = append(out, k)
			}
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok && isAFSOwned(k) {
			out = append(out, k)
		}
	}
	if _, ok := want[KeyApplied]; !ok {
		if _, present := have[KeyApplied]; present && !slices.Contains(out, KeyApplied) {
			out = append(out, KeyApplied)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// isAFSOwned reports whether AFS sets the key: the afs- system keys, not the marker keys other code owns.
func isAFSOwned(k string) bool {
	switch k {
	case KeyManagedBy, KeyOrphan:
		return false
	}
	return strings.HasPrefix(k, "afs-")
}

// Diff returns the tags to add or change on a resource that has have to reach want.
func Diff(have, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		if cur, ok := have[k]; !ok || cur != v {
			out[k] = v
		}
	}
	return out
}

// Routes routes the tag tables to the team resource: a team's own tags reconcile that team, and a change to
// the service tags (fixed values, required keys, defaults) reconciles every team (RC-9). The agent kind
// already routes the same tables (see the agent routes); the routes merge per table.
func Routes() []engine.Route {
	return []engine.Route{
		{Table: "team_tags", Targets: []engine.Target{{Kind: engine.KindTeam}}},
		{Table: "service_tags", FanOut: []engine.Kind{engine.KindTeam}},
	}
}
