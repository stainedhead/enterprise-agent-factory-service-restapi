package mcp

import "strings"

// Level is where a policy rule or enablement is set. Effective access is the intersection of all three.
type Level string

// Levels, widest first.
const (
	LevelService Level = "service"
	LevelTeam    Level = "team"
	LevelAgent   Level = "agent"
)

// Effect is what a rule does.
type Effect string

// Effects.
const (
	EffectAllow Effect = "allow"
	EffectBlock Effect = "block"
)

// Class is an annotation class a rule may select (PRD §7.3).
type Class string

// Annotation classes.
const (
	ClassReadOnly    Class = "read_only"
	ClassDestructive Class = "destructive"
	ClassOpenWorld   Class = "open_world"
)

// Rule is one policy rule. Exactly one of Name (exact or glob, "*" matches any run of characters) and
// Class is set. RateLimit, when above zero, caps calls per minute per agent for matching primitives.
type Rule struct {
	Name      string
	Class     Class
	Effect    Effect
	RateLimit int
}

// Valid reports whether the rule selects something and has a known effect.
func (r Rule) Valid() bool {
	if (r.Name == "") == (r.Class == "") || r.RateLimit < 0 {
		return false
	}
	switch r.Class {
	case "", ClassReadOnly, ClassDestructive, ClassOpenWorld:
	default:
		return false
	}
	return r.Effect == EffectAllow || r.Effect == EffectBlock
}

// specificity: exact name 3, glob 2, class 1, no match 0.
func (r Rule) match(p Primitive, trusted bool) int {
	switch {
	case r.Class != "":
		if !trusted || p.Kind != KindTool {
			return 0
		}
		a := p.Annotations
		if r.Class == ClassReadOnly && a.ReadOnly() || r.Class == ClassDestructive && a.Destructive() ||
			r.Class == ClassOpenWorld && a.OpenWorld() {
			return 1
		}
	case !strings.Contains(r.Name, "*"):
		if r.Name == p.Name {
			return 3
		}
	case Glob(r.Name, p.Name):
		return 2
	}
	return 0
}

// Glob matches s against a pattern where "*" matches any run of characters, including "/" and none.
func Glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, last)
}

// LevelPolicy is the rules at one level for one server.
type LevelPolicy struct {
	Rules []Rule
	// ReadOnly limits the server to read-only tools plus resources and prompts (team or agent switch).
	ReadOnly bool
}

// decide returns whether the level blocks p and the tightest rate limit among the rules that apply.
// Within a level the most specific matching rule wins; at equal specificity a block wins.
func (lp LevelPolicy) decide(p Primitive, trusted bool) (blocked bool, rate int) {
	best, bestBlock := 0, false
	for _, r := range lp.Rules {
		s := r.match(p, trusted)
		if s == 0 {
			continue
		}
		if r.RateLimit > 0 && (rate == 0 || r.RateLimit < rate) {
			rate = r.RateLimit
		}
		switch {
		case s > best:
			best, bestBlock = s, r.Effect == EffectBlock
		case s == best && r.Effect == EffectBlock:
			bestBlock = true
		}
	}
	if lp.ReadOnly && p.Kind == KindTool && !p.Annotations.ReadOnly() {
		return true, rate
	}
	return best > 0 && bestBlock, rate
}

// Reason is why access was refused, in the order the gateway checks (PRD §7.2).
type Reason string

// Refusal reasons.
const (
	ReasonServerNotPublished       Reason = "server_not_published"
	ReasonServerNotEnabledForTeam  Reason = "server_not_enabled_for_team"
	ReasonServerNotEnabledForAgent Reason = "server_not_enabled_for_agent"
	ReasonBlockedService           Reason = "primitive_blocked_service"
	ReasonBlockedTeam              Reason = "primitive_blocked_team"
	ReasonBlockedAgent             Reason = "primitive_blocked_agent"
	ReasonPendingReview            Reason = "primitive_pending_review"
)

// Access is what is known about one server for one agent.
type Access struct {
	Published        bool // service server published, or team server approved and not suspended
	EnabledForTeam   bool
	EnabledForAgent  bool // agent mcp.access all, or the server is selected
	TrustAnnotations bool // review confirmed the annotations
	Service          LevelPolicy
	Team             LevelPolicy
	Agent            LevelPolicy
}

// Decision is the result of Evaluate.
type Decision struct {
	Allowed   bool
	Reason    Reason
	RateLimit int // calls per minute per agent; 0 means none
}

// Evaluate decides whether an agent may list and call p. A block at any level wins.
func (a Access) Evaluate(p Primitive) Decision {
	switch {
	case !a.Published:
		return Decision{Reason: ReasonServerNotPublished}
	case !a.EnabledForTeam:
		return Decision{Reason: ReasonServerNotEnabledForTeam}
	case !a.EnabledForAgent:
		return Decision{Reason: ReasonServerNotEnabledForAgent}
	}
	rate := 0
	for _, l := range []struct {
		lp     LevelPolicy
		reason Reason
	}{{a.Service, ReasonBlockedService}, {a.Team, ReasonBlockedTeam}, {a.Agent, ReasonBlockedAgent}} {
		blocked, r := l.lp.decide(p, a.TrustAnnotations)
		if blocked {
			return Decision{Reason: l.reason}
		}
		if r > 0 && (rate == 0 || r < rate) {
			rate = r
		}
	}
	if p.Status == StatusBlocked {
		return Decision{Reason: ReasonBlockedService}
	}
	if p.Status != StatusApproved {
		return Decision{Reason: ReasonPendingReview}
	}
	return Decision{Allowed: true, RateLimit: rate}
}
