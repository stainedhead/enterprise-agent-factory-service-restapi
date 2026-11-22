package domain

import (
	"regexp"
	"sort"
)

var (
	accountIDRE = regexp.MustCompile(`^[0-9]{12}$`)
	arnAccount  = regexp.MustCompile(`^(arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:)([0-9]{12})(:.*)$`)
)

// ValidateAccountMap checks an import's account map: source and target are 12 digit account ids.
func ValidateAccountMap(m map[string]string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var fields []FieldError
	for _, k := range keys {
		if !accountIDRE.MatchString(k) || !accountIDRE.MatchString(m[k]) {
			fields = append(fields, FieldError{Field: "accountMap." + k, Reason: "source and target must be 12 digit AWS account ids"})
		}
	}
	if len(fields) > 0 {
		return Validation(fields...)
	}
	return nil
}

// RewriteAccounts returns a copy of the team with every AWS account id replaced through the account
// map (RA-US-14.3): the exact account ids of the allowed-accounts filter, the account of each secret
// ARN, of each identity reference and of each role link and its role ARN. An account id the map does not
// name is an error, so the team is reported failed and nothing is written for it (RA-US-14.5, F-8).
// Glob patterns in the filter are kept.
func RewriteAccounts(t TeamBundle, m map[string]string) (TeamBundle, error) {
	var missing error
	mapID := func(id string) string {
		to, ok := m[id]
		if !ok {
			if missing == nil {
				missing = Validation(FieldError{Field: "accountMap", Reason: "account " + id + " is not in the account map"})
			}
			return id
		}
		return to
	}
	// mapValue rewrites an ARN or a bare account id; anything else is not an account reference.
	mapValue := func(v any) any {
		s, ok := v.(string)
		if !ok {
			return v
		}
		if accountIDRE.MatchString(s) {
			return mapID(s)
		}
		if g := arnAccount.FindStringSubmatch(s); g != nil {
			return g[1] + mapID(g[2]) + g[3]
		}
		return v
	}
	rewrite := func(rows []Row, keys ...string) []Row {
		out := make([]Row, 0, len(rows))
		for _, r := range rows {
			c := Row{}
			for k, v := range r {
				c[k] = v
			}
			for _, k := range keys {
				if v, ok := c[k]; ok {
					c[k] = mapValue(v)
				}
			}
			out = append(out, c)
		}
		return out
	}

	out := t
	out.Team = Row{}
	for k, v := range t.Team {
		out.Team[k] = v
	}
	switch list := t.Team["allowed_aws_accounts"].(type) {
	case []any:
		mapped := make([]any, 0, len(list))
		for _, e := range list {
			mapped = append(mapped, mapValue(e))
		}
		out.Team["allowed_aws_accounts"] = mapped
	case []string:
		mapped := make([]any, 0, len(list))
		for _, e := range list {
			mapped = append(mapped, mapValue(e))
		}
		out.Team["allowed_aws_accounts"] = mapped
	}
	out.Secrets = rewrite(t.Secrets, "secrets_manager_arn")
	out.Agents = make([]AgentBundle, 0, len(t.Agents))
	for _, a := range t.Agents {
		a.Identities = rewrite(a.Identities, "external_ref")
		a.RoleLinks = rewrite(a.RoleLinks, "account_id", "role_arn")
		out.Agents = append(out.Agents, a)
	}
	return out, missing
}
