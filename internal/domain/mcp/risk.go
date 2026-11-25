package mcp

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Finding is one result of a definition check (PRD §7.4). Findings never carry the offending text in
// full, only a short reason, so they can be stored and shown safely.
type Finding struct {
	Primitive string `json:"primitive"` // qualified name
	Check     string `json:"check"`
	Detail    string `json:"detail"`
}

// Definition checks.
const (
	CheckModelInstruction = "model_instruction"
	CheckHiddenUnicode    = "hidden_unicode"
	CheckCrossServerRef   = "cross_server_reference"
	CheckCredentialAsk    = "credential_request" //nolint:gosec // a check name, not a credential
	CheckNameCollision    = "name_collision"
)

var (
	instructionRE = regexp.MustCompile(`(?i)(ignore (all |any )?(previous|prior|above) (instructions|prompts?)|disregard (the )?(system|previous)|you must (always|never)|do not (tell|inform|mention to) the user|<\s*(important|system|instructions?)\s*>|system prompt)`)
	credentialRE  = regexp.MustCompile(`(?i)(api[_ -]?key|access[_ -]?token|password|secret[_ -]?key|private[_ -]?key|ssh key|\.aws/credentials|id_rsa|bearer token)`)
)

// HiddenRune reports whether r is invisible or reorders text: zero-width characters, bidirectional
// controls, Unicode tag characters and other format characters.
func HiddenRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x2069, r == 0xFEFF, r == 0x00AD, r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// CheckDefinitions runs the definition checks over a server's primitives. otherServers are the slugs
// of other servers the team can reach; a description that names one of their tools is flagged.
// Credential requests are flagged in descriptions only, not in parameter names.
func CheckDefinitions(prims []Primitive, otherServers []string) []Finding {
	var out []Finding
	seen := map[string]bool{}
	for _, p := range prims {
		q := p.QualifiedName()
		if seen[q] {
			out = append(out, Finding{q, CheckNameCollision, "duplicate name within the server"})
		}
		seen[q] = true
		text := p.Title + "\n" + p.Description
		if instructionRE.MatchString(text) {
			out = append(out, Finding{q, CheckModelInstruction, "description contains instructions aimed at the model"})
		}
		if strings.IndexFunc(text+p.Name, HiddenRune) >= 0 {
			out = append(out, Finding{q, CheckHiddenUnicode, "invisible or bidirectional characters"})
		}
		if credentialRE.MatchString(p.Description) {
			out = append(out, Finding{q, CheckCredentialAsk, "description refers to credentials"})
		}
		for _, s := range otherServers {
			if s != p.Server && s != "" && strings.Contains(text, s+Separator) {
				out = append(out, Finding{q, CheckCrossServerRef, "refers to another server's tools: " + s})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primitive != out[j].Primitive {
			return out[i].Primitive < out[j].Primitive
		}
		return out[i].Check < out[j].Check
	})
	return out
}

// Location is where a server runs (PRD §7.1).
type Location string

// Locations.
const (
	LocationInNamespace Location = "in_namespace" // hosted by AFS in the team namespace
	LocationCorporate   Location = "corporate"    // remote, on an allowed corporate domain
	LocationExternal    Location = "external"     // remote, public internet
)

// Valid reports whether l is a known location.
func (l Location) Valid() bool {
	return l == LocationInNamespace || l == LocationCorporate || l == LocationExternal
}

// Classification is a data classification, lowest first.
type Classification string

// Classifications.
const (
	ClassPublic       Classification = "public"
	ClassInternal     Classification = "internal"
	ClassConfidential Classification = "confidential"
	ClassRestricted   Classification = "restricted"
)

func (c Classification) rank() int {
	switch c {
	case ClassPublic:
		return 0
	case ClassInternal:
		return 1
	case ClassConfidential:
		return 2
	}
	return 3
}

// RiskInput is what the tier is computed from.
type RiskInput struct {
	Location       Location
	Host           string
	HostDenyList   []string // glob patterns, "*" matches one or more labels
	AuthKind       CredentialKind
	Classification Classification
	TeamCeiling    Classification
	Primitives     []Primitive
	Findings       []Finding
}

// RiskResult is a tier with its reasons, in a stable order.
type RiskResult struct {
	Tier    Tier     `json:"tier"`
	Reasons []string `json:"reasons"`
}

// Assess computes the risk tier of a server (PRD §7.5). Any failed definition check, a denied host or
// a classification above the team's ceiling makes it high. External location, a destructive or
// open-world tool, or a user-delegated credential makes it at least elevated.
func Assess(in RiskInput) RiskResult {
	r := RiskResult{Tier: TierLow}
	raise := func(t Tier, why string) {
		r.Tier = Max(r.Tier, t)
		r.Reasons = append(r.Reasons, why)
	}
	if in.Host != "" && HostDenied(in.Host, in.HostDenyList) {
		raise(TierHigh, "host_denied")
	}
	if len(in.Findings) > 0 {
		raise(TierHigh, "definition_checks_failed")
	}
	if in.TeamCeiling != "" && in.Classification.rank() > in.TeamCeiling.rank() {
		raise(TierHigh, "classification_above_team_ceiling")
	}
	if in.Location != LocationInNamespace && in.Location != LocationCorporate {
		raise(TierElevated, "external_location")
	}
	if in.AuthKind == CredOAuthUser {
		raise(TierElevated, "user_delegated_credential")
	}
	var destructive, open bool
	for _, p := range in.Primitives {
		if p.Kind != KindTool {
			continue
		}
		destructive = destructive || p.Annotations.Destructive()
		open = open || p.Annotations.OpenWorld()
	}
	if destructive {
		raise(TierElevated, "destructive_tools")
	}
	if open {
		raise(TierElevated, "open_world_tools")
	}
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	return r
}

// HostDenied reports whether host matches any pattern. Matching is case-insensitive; a leading "*."
// matches one or more labels, so "*.example.com" matches "a.b.example.com" but not "example.com".
func HostDenied(host string, patterns []string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "":
		case strings.HasPrefix(p, "*."):
			if strings.HasSuffix(h, p[1:]) && len(h) > len(p)-1 {
				return true
			}
		case h == p:
			return true
		}
	}
	return false
}
