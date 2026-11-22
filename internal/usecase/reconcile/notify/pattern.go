package notify

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// RulePrefix prefixes every rule, connection and API destination name of a channel; the rest is the
// channel id, so the owner of any of them is known from its name (sweep, DLQ routing).
const RulePrefix = "afs-"

// ObjectName is the name of a channel's rule, connection and API destination.
func ObjectName(channelID string) string { return RulePrefix + channelID }

// ChannelIDOf returns the channel id of an object name made by ObjectName.
func ChannelIDOf(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, RulePrefix+string(domain.PrefixChannel)+"_")
	if !ok || id == "" {
		return "", false
	}
	return string(domain.PrefixChannel) + "_" + id, true
}

// defaultFamilies are the event families a channel with no eventTypes receives: every family of
// contract §7 except notification.* (a failing channel must not be fed its own failures) and report.*
// (delivered only to the channel a schedule addresses, OQ-S2).
func defaultFamilies() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range domain.EventTypes() {
		fam, _, _ := strings.Cut(string(t), ".")
		if fam == "notification" || fam == "report" || seen[fam] {
			continue
		}
		seen[fam] = true
		out = append(out, fam+".")
	}
	sort.Strings(out)
	return out
}

// EventPattern is the rule's event pattern for a channel: the AFS events of the channel's team whose
// type the channel subscribed to (all families when none), or any report.* event addressed to the
// channel (OQ-S2). A team channel's pattern always carries the team id, so it never matches another
// team's events; a service-level channel matches its listed event types for every team.
func EventPattern(ch Channel) (string, error) {
	var types []any
	if len(ch.EventTypes) == 0 {
		for _, f := range defaultFamilies() {
			types = append(types, map[string]string{"prefix": f})
		}
	}
	for _, f := range ch.EventTypes {
		if p, ok := strings.CutSuffix(f, ".*"); ok {
			types = append(types, map[string]string{"prefix": p + "."})
		} else {
			types = append(types, f)
		}
	}
	own := map[string]any{"detail-type": types}
	if ch.TeamID != "" {
		own["detail"] = map[string]any{"teamId": []string{ch.TeamID}}
	}
	report := map[string]any{
		"detail-type": []any{map[string]string{"prefix": "report."}},
		"detail":      map[string]any{"data": map[string]any{"channelId": []string{ch.ID}}},
	}
	if ch.TeamID != "" {
		report["detail"].(map[string]any)["teamId"] = []string{ch.TeamID}
	}
	b, err := json.Marshal(map[string]any{"source": []string{domain.EventSource}, "$or": []any{own, report}})
	return string(b), err
}
