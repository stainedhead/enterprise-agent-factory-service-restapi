package gateway

import (
	"regexp"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// MaxRequestMetadata is Bedrock's limit on requestMetadata pairs (research OQ-2; gateway contract §6).
const MaxRequestMetadata = 16

// MaxBodyBytes is the largest request body the gateway accepts (gateway contract §7: 413 above it).
const MaxBodyBytes = 20 << 20

// modelIDRE is the format of an AFS model id (a Bedrock model id, with the ":0" version suffix).
var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)

// ValidModelID reports whether s looks like an AFS model id; anything else is a 400.
func ValidModelID(s string) bool { return modelIDRE.MatchString(s) }

// Streams reports whether op answers with a stream of events. The Anthropic path streams only when the
// body asks for it, so the adapter picks the stream form (Request.Op is then OpAnthropicMessages and
// the adapter decides).
func (o Operation) Streams() bool { return o == OpConverseStream || o == OpInvokeStream }

// Known reports whether o is a supported operation.
func (o Operation) Known() bool {
	switch o {
	case OpConverse, OpConverseStream, OpInvoke, OpInvokeStream, OpAnthropicMessages:
		return true
	}
	return false
}

// Tag keys of the system tags every call carries (gateway contract §6).
const (
	TagTeamID  = "afs-team-id"
	TagAgentID = "afs-agent-id"
	TagPoolID  = "afs-pool-id"
)

// BuildMetadata returns the requestMetadata of a call: system tags first (team, then agent or pool),
// then service fixed and required tags, then team tags in key order, at most 16 pairs (D27). The keys
// that did not fit are returned so the team's cost tags page can list them. Whatever the agent sent in
// requestMetadata is not an input: it is replaced (CT-GW-7).
func BuildMetadata(c Caller, tags []Tag) (meta map[string]string, dropped []string) {
	meta = map[string]string{TagTeamID: c.TeamID}
	order := []string{TagTeamID}
	if c.AgentID != "" {
		meta[TagAgentID] = c.AgentID
		order = append(order, TagAgentID)
	} else if c.PoolID != "" {
		meta[TagPoolID] = c.PoolID
		order = append(order, TagPoolID)
	}
	rank := func(source string) int {
		switch source {
		case "service", "service_default":
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
		if ri, rj := rank(rest[i].Source), rank(rest[j].Source); ri != rj {
			return ri < rj
		}
		return rest[i].Key < rest[j].Key
	})
	for _, t := range rest {
		if _, dup := meta[t.Key]; dup {
			continue
		}
		if len(order) >= MaxRequestMetadata {
			dropped = append(dropped, t.Key)
			continue
		}
		meta[t.Key] = t.Value
		order = append(order, t.Key)
	}
	return meta, dropped
}

// TeamRoleARN is the ARN of the team's Bedrock role, which only the gateway's role may assume. The
// partition and account are the gateway's own (AFS_GATEWAY_ROLE_ARN).
func TeamRoleARN(partition, accountID, namespace, teamID string) (string, error) {
	name, err := team.BedrockRoleName(teamID)
	if err != nil {
		return "", err
	}
	return team.RoleARN(partition, accountID, team.RolePath(namespace), name), nil
}

// PeriodStart returns the first instant of the budget's current period: the month (UTC) of now for a
// monthly budget, the budget's start for a lifetime one (database contract §4.2).
func PeriodStart(b BudgetRecord, now time.Time) time.Time {
	if b.Period == "monthly" {
		n := now.UTC()
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return b.StartsAt.UTC()
}

func denied(reason, msg string) *domain.Error {
	e := &domain.Error{Kind: domain.KindAccessDenied, Message: msg, Reason: reason}
	if reason != "" {
		e.Details = map[string]any{"reason": reason}
	}
	return e
}

func unauthenticated(msg string) *domain.Error {
	return &domain.Error{Kind: domain.KindUnauthenticated, Message: msg}
}

func unavailable(msg string, cause error) *domain.Error {
	return &domain.Error{Kind: domain.KindUnavailable, Message: msg, Cause: cause}
}
