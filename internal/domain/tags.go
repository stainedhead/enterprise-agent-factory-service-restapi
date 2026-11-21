package domain

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// MaxEffectiveTags caps the effective tag set of a team (D27, OQ-2).
const MaxEffectiveTags = 40

// MaxRequestMetadataPairs is the Bedrock requestMetadata limit (D27).
const MaxRequestMetadataPairs = 16

var (
	tagKeyRE   = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	tagValueRE = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,256}$`)
	tagSecret  = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|PRIVATE.?KEY|API.?KEY|ACCESS.?KEY|CREDENTIAL)`)
)

// ValidTagKey mirrors afs.tag_key_ok: AWS tag rules minus reserved prefixes and credential-like keys.
func ValidTagKey(k string) bool {
	l := strings.ToLower(k)
	return tagKeyRE.MatchString(k) && !strings.HasPrefix(l, "aws:") && !strings.HasPrefix(l, "afs-") &&
		!strings.HasPrefix(l, "afs:") && !tagSecret.MatchString(k)
}

// ValidTagValue mirrors afs.tag_value_ok.
func ValidTagValue(v string) bool { return tagValueRE.MatchString(v) }

// ServiceTag is a service-level tag: fixed (one value for every team) or required (every team gives
// its own value, optionally from a list or with a default).
type ServiceTag struct {
	Key           string
	Mode          ServiceTagMode
	Value         *string
	DefaultValue  *string
	AllowedValues []string
	Description   string
	Version       Version
	UpdatedAt     time.Time
}

// Validate checks the shape the database also enforces.
func (t ServiceTag) Validate() error {
	var fe []FieldError
	bad := func(f, r string) { fe = append(fe, FieldError{Field: f, Reason: r}) }
	if !ValidTagKey(t.Key) {
		bad("key", "is not a valid tag key (no aws:, afs- or afs: prefix, no credential-like words)")
	}
	if !t.Mode.Valid() {
		bad("mode", "must be fixed or required")
	}
	if len(t.Description) > 500 {
		bad("description", "must be at most 500 characters")
	}
	switch t.Mode {
	case ServiceTagModeFixed:
		if t.Value == nil || !ValidTagValue(*t.Value) {
			bad("value", "is required for a fixed tag and must be a valid tag value")
		}
		if t.DefaultValue != nil || t.AllowedValues != nil {
			bad("defaultValue", "defaultValue and allowedValues are only for a required tag")
		}
	case ServiceTagModeRequired:
		if t.Value != nil {
			bad("value", "is only for a fixed tag")
		}
		if t.DefaultValue != nil && !ValidTagValue(*t.DefaultValue) {
			bad("defaultValue", "is not a valid tag value")
		}
		if t.AllowedValues != nil {
			if n := len(t.AllowedValues); n < 1 || n > 200 {
				bad("allowedValues", "must hold 1 to 200 values")
			}
			for _, v := range t.AllowedValues {
				if !ValidTagValue(v) {
					bad("allowedValues", v+" is not a valid tag value")
				}
			}
			if t.DefaultValue != nil && !slices.Contains(t.AllowedValues, *t.DefaultValue) {
				bad("defaultValue", "must be one of allowedValues")
			}
		}
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// TagValue is one team-owned tag value.
type TagValue struct{ Key, Value string }

// Tag is one effective tag with where it came from.
type Tag struct {
	Key    string
	Value  string
	Source TagSource
}

// TagSet is the effective tag set of a team: system, service and team tags (API contract §5).
type TagSet struct{ Tags []Tag }

// EffectiveTagSet builds a team's effective tags: afs-team-id, fixed service tags, required service
// tags (the team's value, else the default, else missing) and the team's own tags, in key order
// within each group.
func EffectiveTagSet(teamID string, service []ServiceTag, team []TagValue) TagSet {
	own := map[string]string{}
	for _, t := range team {
		own[t.Key] = t.Value
	}
	svcKeys := map[string]bool{}
	svc := slices.Clone(service)
	sort.Slice(svc, func(i, j int) bool { return svc[i].Key < svc[j].Key })
	out := []Tag{{Key: "afs-team-id", Value: teamID, Source: TagSourceSystem}}
	for _, s := range svc {
		svcKeys[s.Key] = true
		switch {
		case s.Mode == ServiceTagModeFixed && s.Value != nil:
			out = append(out, Tag{s.Key, *s.Value, TagSourceService})
		case s.Mode == ServiceTagModeRequired:
			if v, ok := own[s.Key]; ok {
				out = append(out, Tag{s.Key, v, TagSourceTeam})
			} else if s.DefaultValue != nil {
				out = append(out, Tag{s.Key, *s.DefaultValue, TagSourceServiceDefault})
			} else {
				out = append(out, Tag{Key: s.Key, Source: TagSourceMissing})
			}
		}
	}
	keys := make([]string, 0, len(own))
	for k := range own {
		if !svcKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, Tag{k, own[k], TagSourceTeam})
	}
	return TagSet{Tags: out}
}

// BedrockMetadata returns the first max (at most 16) tags that have a value, in effective order
// (system, service, team), and the keys left out because of the limit (D27).
func (s TagSet) BedrockMetadata(limit int) (pairs []Tag, dropped []string) {
	if limit <= 0 || limit > MaxRequestMetadataPairs {
		limit = MaxRequestMetadataPairs
	}
	for _, t := range s.Tags {
		if t.Source == TagSourceMissing {
			continue
		}
		if len(pairs) < limit {
			pairs = append(pairs, t)
		} else {
			dropped = append(dropped, t.Key)
		}
	}
	return pairs, dropped
}

// ValidateTeamTags checks a PUT of team-owned tags against the service tags and returns the missing
// required keys (those with no default) in details.missingTags when any remain unset. Fixed keys,
// duplicate keys (case-insensitive), a value outside allowedValues and more than MaxEffectiveTags
// effective keys are refused.
func ValidateTeamTags(service []ServiceTag, values []TagValue) error {
	var fe []FieldError
	bySvc := map[string]ServiceTag{}
	for _, s := range service {
		bySvc[strings.ToLower(s.Key)] = s
	}
	seen := map[string]bool{}
	set := map[string]bool{}
	for _, v := range values {
		l := strings.ToLower(v.Key)
		if seen[l] {
			fe = append(fe, FieldError{Field: "tags", Reason: "key " + v.Key + " is listed twice"})
			continue
		}
		seen[l] = true
		if !ValidTagKey(v.Key) {
			fe = append(fe, FieldError{Field: "tags", Reason: v.Key + " is not a valid tag key"})
		}
		if !ValidTagValue(v.Value) {
			fe = append(fe, FieldError{Field: "tags", Reason: "the value of " + v.Key + " is not a valid tag value"})
		}
		if s, ok := bySvc[l]; ok {
			switch {
			case s.Key != v.Key:
				fe = append(fe, FieldError{Field: "tags", Reason: "tag key " + v.Key + " must be spelled " + s.Key})
			case s.Mode == ServiceTagModeFixed:
				fe = append(fe, FieldError{Field: "tags", Reason: v.Key + " is set by service admins for every team"})
			case s.AllowedValues != nil && !slices.Contains(s.AllowedValues, v.Value):
				fe = append(fe, FieldError{Field: "tags", Reason: "the value of " + v.Key + " must be one of " + strings.Join(s.AllowedValues, ", ")})
			}
		}
		set[v.Key] = true
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	if missing := MissingRequiredTags(service, set); len(missing) > 0 {
		e := Validation(FieldError{Field: "tags", Reason: "missing required tags: " + strings.Join(missing, ", ")})
		e.Details = map[string]any{"missingTags": missing}
		return e
	}
	if n := len(EffectiveTagSet("", service, values).Tags); n-1 > MaxEffectiveTags {
		return Validation(FieldError{Field: "tags", Reason: "a team has at most 40 effective tag keys"})
	}
	return nil
}

// MissingRequiredTags lists required service tags without a default that the team sets no value for.
func MissingRequiredTags(service []ServiceTag, set map[string]bool) []string {
	var out []string
	for _, s := range service {
		if s.Mode == ServiceTagModeRequired && s.DefaultValue == nil && !set[s.Key] {
			out = append(out, s.Key)
		}
	}
	sort.Strings(out)
	return out
}
