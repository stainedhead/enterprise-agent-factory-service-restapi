package gateway

import (
	"regexp"
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
