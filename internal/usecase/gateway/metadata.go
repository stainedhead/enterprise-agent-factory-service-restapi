package gateway

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// BuildMetadata returns the requestMetadata of a call (GW-5, D27): at most MaxRequestMetadata (16) pairs,
// in this order: afs-team-id, afs-agent-id (afs-pool-id for a pool), the service tags (fixed and required,
// whoever set the value) in key order, then the team's own tags in key order. The keys that did not fit are
// returned in that order; GetTeamTags lists the same keys (teams.Service computes them with one pair kept
// for the agent or pool id). Whatever the agent sent in requestMetadata is not an input: it is replaced
// (CT-GW-7). Tags with no value are skipped, and so is a key that repeats a system key.
func BuildMetadata(c Caller, tags []Tag) (meta map[string]string, dropped []string) {
	meta = map[string]string{TagTeamID: c.TeamID}
	n := 1
	switch {
	case c.AgentID != "":
		meta[TagAgentID] = c.AgentID
		n++
	case c.PoolID != "":
		meta[TagPoolID] = c.PoolID
		n++
	}
	rank := func(t Tag) int {
		if t.Service || t.Source == "service" || t.Source == "service_default" {
			return 0
		}
		return 1
	}
	rest := make([]Tag, 0, len(tags))
	for _, t := range tags {
		if t.Source == "system" || t.Value == "" {
			continue
		}
		rest = append(rest, t)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if ri, rj := rank(rest[i]), rank(rest[j]); ri != rj {
			return ri < rj
		}
		return rest[i].Key < rest[j].Key
	})
	for _, t := range rest {
		if _, dup := meta[t.Key]; dup {
			continue
		}
		if n >= MaxRequestMetadata {
			dropped = append(dropped, t.Key)
			continue
		}
		meta[t.Key] = t.Value
		n++
	}
	return meta, dropped
}

// droppedNotes remembers the dropped keys last logged for each team, so a team whose tags do not fit is
// reported when the set changes and not on every call.
type droppedNotes struct {
	mu   sync.Mutex
	last map[string]string
}

// note logs the keys left out of a team's requestMetadata when they differ from the last call's. The
// recorded list for people is GetTeamTags (requestMetadataDroppedKeys).
func (d *droppedNotes) note(log *slog.Logger, teamID string, dropped []string) {
	joined := strings.Join(dropped, ",")
	d.mu.Lock()
	if d.last == nil {
		d.last = map[string]string{}
	}
	changed := d.last[teamID] != joined
	if joined == "" {
		delete(d.last, teamID)
	} else {
		d.last[teamID] = joined
	}
	d.mu.Unlock()
	if changed && joined != "" {
		log.Warn("requestMetadata keys dropped: more than 16 tag pairs", "team", teamID, "dropped", dropped)
	}
}
