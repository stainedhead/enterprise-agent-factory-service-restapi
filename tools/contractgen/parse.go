package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Operation is one row of API contract §4, split per operationId.
type Operation struct {
	OperationID string `json:"operationId"`
	Section     string `json:"section"`
	Method      string `json:"method"`
	// Path is the contract path, with action verbs as in the contract (/v1/teams/{teamId}:suspend).
	Path string `json:"path"`
	// Query lists the query parameters named in the contract's path cell.
	Query []string `json:"query,omitempty"`
	// Permission is the permission the route requires; empty for public and any-signed-in routes.
	Permission string `json:"permission,omitempty"`
	// AnyOf lists alternative permissions: the route is allowed when the caller holds any of them.
	AnyOf []string `json:"anyOf,omitempty"`
	// FieldPermissions maps request fields to the further permission they need.
	FieldPermissions map[string]string `json:"fieldPermissions,omitempty"`
	// Condition names a team flag or relation the permission depends on.
	Condition string `json:"condition,omitempty"`
	// Scope is team, service, public or any.
	Scope string `json:"scope"`
	CLI   string `json:"cli"`
	SDK   string `json:"sdk"`
	// Screen is the console screen list, as written.
	Screen   string `json:"screen"`
	Priority string `json:"priority"`
	CSV      bool   `json:"csv"`
	// Notes is the free text of the path cell after the path (shape hints).
	Notes string `json:"notes,omitempty"`
}

// Grant is one role's grant of a permission, optionally conditional.
type Grant struct {
	Role      string `json:"role"`
	Condition string `json:"condition,omitempty"`
}

// PermissionEntry is one permission of API contract §3 with its grants.
type PermissionEntry struct {
	Name   string  `json:"name"`
	Grants []Grant `json:"grants"`
	Notes  string  `json:"notes,omitempty"`
}

// ErrorEntry is one row of API contract §6.
type ErrorEntry struct {
	HTTP     int    `json:"http"`
	Code     string `json:"code"`
	When     string `json:"when"`
	DBOrigin string `json:"dbOrigin,omitempty"`
	// GatewayOnly marks codes that afs-api never returns.
	GatewayOnly bool `json:"gatewayOnly,omitempty"`
}

// EventEntry is one event type of API contract §7.
type EventEntry struct {
	Type      string   `json:"type"`
	EmittedBy []string `json:"emittedBy"`
	When      string   `json:"when"`
}

// SettingEntry is one service setting of root PRD §6.13.
type SettingEntry struct {
	Key     string `json:"key"`
	Type    string `json:"type"` // string, boolean, integer, array
	Default any    `json:"default"`
	// Nullable is true when null is an allowed value.
	Nullable bool   `json:"nullable,omitempty"`
	Min      *int64 `json:"min,omitempty"`
	Max      *int64 `json:"max,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Allowed  string `json:"allowed"`
	UsedBy   string `json:"usedBy"`
	Since    string `json:"migration"`
	Items    string `json:"items,omitempty"`
}

// roles in contract §3 column order.
var roles = []string{"team_viewer", "team_admin", "service_admin", "security_operator"}

// splitRow splits a markdown table row on unescaped pipes and unescapes \|.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// section returns the lines from the heading line that starts with start up to the next heading
// of the same or a higher level.
func section(lines []string, start string) []string {
	level := strings.IndexByte(start, ' ')
	for i, l := range lines {
		if !strings.HasPrefix(l, start) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if h := headingLevel(lines[j]); h > 0 && h <= level {
				return lines[i:j]
			}
		}
		return lines[i:]
	}
	return nil
}

func headingLevel(l string) int {
	n := 0
	for n < len(l) && l[n] == '#' {
		n++
	}
	if n > 0 && n < len(l) && l[n] == ' ' {
		return n
	}
	return 0
}

// tableRows returns the body rows of every table in lines (header and separator removed).
func tableRows(lines []string) [][]string {
	var rows [][]string
	inTable := false
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "|") {
			inTable = false
			continue
		}
		if !inTable {
			// header row; the next line is the separator
			inTable = true
			continue
		}
		if i > 0 && isSeparator(l) {
			continue
		}
		rows = append(rows, splitRow(l))
	}
	return rows
}

func isSeparator(l string) bool {
	return strings.Trim(strings.TrimSpace(l), "|-: ") == ""
}

var codeRE = regexp.MustCompile("`([^`]*)`")

// codes returns the backtick-quoted spans of s.
func codes(s string) []string {
	var out []string
	for _, m := range codeRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// parsePermissions parses API contract §3.
func parsePermissions(lines []string) ([]PermissionEntry, error) {
	sec := section(lines, "## 3.")
	if sec == nil {
		return nil, fmt.Errorf("§3 not found")
	}
	var out []PermissionEntry
	for _, row := range tableRows(sec) {
		if len(row) != 6 {
			return nil, fmt.Errorf("§3 row has %d cells: %v", len(row), row)
		}
		groups := strings.Split(row[0], "/")
		cells := make([][]string, len(roles))
		for r := range roles {
			cells[r] = strings.Split(row[1+r], "/")
			for len(cells[r]) < len(groups) {
				cells[r] = append(cells[r], "")
			}
		}
		for g, group := range groups {
			for _, name := range codes(group) {
				e := PermissionEntry{Name: name, Notes: row[5], Grants: []Grant{}}
				for r, role := range roles {
					cell := strings.TrimSpace(cells[r][g])
					if !strings.HasPrefix(cell, "✓") {
						continue
					}
					e.Grants = append(e.Grants, Grant{Role: role, Condition: grantCondition(cell)})
				}
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// grantCondition turns "✓ when `x`", "✓ assigned or own repos", "✓ on the assigned team" and
// "✓ all" into a condition name ("" when unconditional).
func grantCondition(cell string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(cell, "✓"))
	switch {
	case rest == "" || rest == "all":
		return ""
	case strings.HasPrefix(rest, "when "):
		if c := codes(rest); len(c) == 1 {
			return c[0]
		}
		return strings.TrimPrefix(rest, "when ")
	case rest == "assigned or own repos":
		return "assignedOrOwnRepos"
	case rest == "on the assigned team":
		return "assignedTeam"
	}
	return rest
}

var (
	methodRE = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE)(?:/(GET|POST|PUT|PATCH|DELETE))? (\S+)$`)
	// twoMethodRE matches the contract's `GET`/`PUT /path` form.
	twoMethodRE = regexp.MustCompile("`(GET|POST|PUT|PATCH|DELETE)`/`(GET|POST|PUT|PATCH|DELETE) ")
	queryVarRE  = regexp.MustCompile(`[?&]([A-Za-z]+)`)
)

// parseOperations parses every table of API contract §4.
func parseOperations(lines []string) ([]Operation, error) {
	sec := section(lines, "## 4.")
	if sec == nil {
		return nil, fmt.Errorf("§4 not found")
	}
	var ops []Operation
	current := ""
	svcSection := false
	for i := 0; i < len(sec); i++ {
		l := sec[i]
		if strings.HasPrefix(l, "### ") {
			current = strings.Fields(l)[1]
			svcSection = strings.Contains(l, "Service administration")
			continue
		}
		if !strings.HasPrefix(l, "| `") {
			continue
		}
		row := splitRow(l)
		if len(row) != 7 {
			return nil, fmt.Errorf("§4 row has %d cells: %s", len(row), l)
		}
		rowOps, err := expandRow(row, current, svcSection)
		if err != nil {
			return nil, err
		}
		ops = append(ops, rowOps...)
	}
	return ops, nil
}

// expandRow splits a row that names several operations ("A / B") into one Operation each.
func expandRow(row []string, sec string, svcSection bool) ([]Operation, error) {
	ids := codes(row[0])
	pathCodes, notes := splitPathCell(row[1])
	if len(pathCodes) == 0 {
		return nil, fmt.Errorf("no path in %q", row[1])
	}
	type mp struct{ method, path string }
	var mps []mp
	first := pathCodes[0]
	if m := methodRE.FindStringSubmatch(first); m != nil && m[2] != "" {
		// `GET`/`PUT /path`: two methods on one path
		mps = []mp{{m[1], m[3]}, {m[2], m[3]}}
	} else {
		parts := strings.SplitN(first, " ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad method and path %q", first)
		}
		mps = append(mps, mp{parts[0], parts[1]})
		for _, extra := range pathCodes[1:] {
			if !strings.HasPrefix(extra, ":") {
				continue
			}
			base := mps[0].path
			if k := strings.LastIndex(base, ":"); k > strings.LastIndex(base, "/") {
				base = base[:k]
			}
			mps = append(mps, mp{mps[0].method, base + extra})
		}
	}
	if len(mps) != len(ids) {
		// The first code of the path cell may be followed by codes that are not paths (bodies).
		if len(ids) == 1 {
			mps = mps[:1]
		} else {
			return nil, fmt.Errorf("%v: %d operations but %d paths", ids, len(ids), len(mps))
		}
	}
	perms := permAlternatives(row[2], len(ids))
	clis := expandList(row[3], len(ids), " ")
	sdks := expandList(row[4], len(ids), ".")
	csv := strings.Contains(row[1], "*CSV*")
	var out []Operation
	for i, id := range ids {
		path, query := cleanPath(mps[i].path)
		op := Operation{
			OperationID: id, Section: sec, Method: mps[i].method, Path: path, Query: query,
			CLI: clis[i], SDK: sdks[i], Screen: row[5], Priority: row[6], CSV: csv, Notes: notes,
		}
		applyPermission(&op, perms[i], svcSection)
		out = append(out, op)
	}
	return out, nil
}

// splitPathCell returns the backtick spans of the method/path cell (paths and action suffixes)
// and the remaining free text.
func splitPathCell(cell string) ([]string, string) {
	cell = twoMethodRE.ReplaceAllString(cell, "`$1/$2 ")
	cs := codes(cell)
	var paths []string
	for _, c := range cs {
		if methodRE.MatchString(c) || strings.HasPrefix(c, ":") {
			paths = append(paths, c)
		}
	}
	notes := strings.TrimSpace(codeRE.ReplaceAllStringFunc(cell, func(s string) string {
		if slices.Contains(paths, strings.Trim(s, "`")) {
			return ""
		}
		return s
	}))
	notes = strings.TrimSpace(strings.Trim(strings.ReplaceAll(notes, "*CSV*", ""), " /"))
	return paths, notes
}

var reportKindRE = regexp.MustCompile(`\{[a-z]+(\|[a-z]+)+\}`)

// cleanPath strips the query string and turns enumerations in braces into {kind}.
func cleanPath(p string) (string, []string) {
	var query []string
	if k := strings.IndexByte(p, '?'); k >= 0 {
		for _, m := range queryVarRE.FindAllStringSubmatch(p[k:], -1) {
			query = append(query, m[1])
		}
		p = p[:k]
	}
	p = reportKindRE.ReplaceAllString(p, "{kind}")
	return p, query
}

// expandList splits "teams suspend-team / resume-team" into full values per operation. For CLI
// values (sep " ") a short value gets the service word; for SDK values (sep ".") the object path.
func expandList(cell string, n int, sep string) []string {
	vals := codes(cell)
	if len(vals) == 0 {
		vals = []string{strings.TrimSpace(cell)}
	}
	out := make([]string, n)
	for i := range n {
		v := vals[min(i, len(vals)-1)]
		if i > 0 && i < len(vals) && !strings.Contains(v, sep) {
			base := vals[0]
			if k := strings.LastIndex(base, sep); k >= 0 {
				v = base[:k+1] + v
			}
		}
		out[i] = v
	}
	return out
}

// permAlternatives splits the permission cell per operation ("`a` / `b`").
func permAlternatives(cell string, n int) []string {
	if n == 1 {
		return []string{cell}
	}
	parts := strings.Split(cell, " / ")
	out := make([]string, n)
	for i := range n {
		out[i] = strings.TrimSpace(parts[min(i, len(parts)-1)])
	}
	return out
}

// applyPermission sets the permission, alternatives, field permissions, condition and scope.
func applyPermission(op *Operation, cell string, svcSection bool) {
	cs := codes(cell)
	svc := strings.Contains(cell, "*svc*") || (svcSection && !strings.Contains(op.Notes, "not *svc*"))
	switch {
	case strings.HasPrefix(cell, "none"):
		op.Scope = "public"
	case strings.HasPrefix(cell, "any signed-in"):
		op.Scope = "any"
	default:
		op.Permission = cs[0]
	}
	switch op.OperationID {
	case "UpdateTeam":
		op.FieldPermissions = map[string]string{"guardrails": cs[1], "allowedAwsAccounts": cs[3]}
	case "DeclareTeamExternalSystem":
		op.Condition = cs[1]
	case "UpdateWorkOrderRepo":
		op.AnyOf = []string{cs[0], cs[1]}
		op.Condition = "assignedTeam"
	case "ListWorkOrders":
		op.Condition = "assignedOrOwnRepos"
	}
	if strings.Contains(op.Notes, "`onExhaust: notify` needs `teams:set-guardrails`") ||
		(op.OperationID == "PutBudget" && strings.Contains(op.Notes, "teams:set-guardrails")) {
		op.FieldPermissions = map[string]string{"onExhaust=notify": "teams:set-guardrails"}
	}
	if op.Scope != "" {
		return
	}
	switch {
	case strings.HasPrefix(op.Path, "/v1/teams/{teamId}"):
		op.Scope = "team"
	case svc:
		op.Scope = "service"
	default:
		op.Scope = "any"
	}
}

// parseErrors parses API contract §6.
func parseErrors(lines []string) ([]ErrorEntry, error) {
	sec := section(lines, "## 6.")
	var out []ErrorEntry
	for _, row := range tableRows(sec) {
		if len(row) != 4 {
			continue
		}
		var status int
		if _, err := fmt.Sscanf(row[0], "%d", &status); err != nil {
			return nil, fmt.Errorf("§6 status %q: %w", row[0], err)
		}
		c := codes(row[1])
		if len(c) != 1 {
			return nil, fmt.Errorf("§6 code %q", row[1])
		}
		origin := row[3]
		if origin == "—" {
			origin = ""
		}
		out = append(out, ErrorEntry{
			HTTP: status, Code: c[0], When: row[2], DBOrigin: origin,
			GatewayOnly: strings.Contains(row[2], "afs-gateway"),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("§6 has no rows")
	}
	return out, nil
}

// parseEvents parses API contract §7.
func parseEvents(lines []string) ([]EventEntry, error) {
	sec := section(lines, "## 7.")
	var out []EventEntry
	for _, row := range tableRows(sec) {
		if len(row) != 3 {
			continue
		}
		var by []string
		for _, b := range strings.Split(row[1], ",") {
			b = strings.TrimSpace(b)
			b = strings.TrimSuffix(b, " collectors")
			if b != "" {
				by = append(by, b)
			}
		}
		for _, t := range codes(row[0]) {
			out = append(out, EventEntry{Type: t, EmittedBy: by, When: row[2]})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("§7 has no rows")
	}
	return out, nil
}

var rangeRE = regexp.MustCompile(`^(-?\d+) to (-?\d+)`)

// parseSettings parses root PRD §6.13.
func parseSettings(lines []string) ([]SettingEntry, error) {
	sec := section(lines, "### 6.13")
	var out []SettingEntry
	for _, row := range tableRows(sec) {
		if len(row) != 5 {
			continue
		}
		if strings.TrimSpace(row[1]) == "unset" {
			continue // a database-only key (CD-8): in §6.13, never exposed by GetSettings
		}
		key := codes(row[0])
		def := codes(row[1])
		if len(key) != 1 || len(def) != 1 {
			return nil, fmt.Errorf("§6.13 row %v", row)
		}
		s := SettingEntry{Key: key[0], Allowed: row[2], UsedBy: row[3], Since: row[4]}
		allowed := row[2]
		switch d := def[0]; {
		case d == "true" || d == "false":
			s.Type, s.Default = "boolean", d == "true"
		case d == "null":
			s.Type, s.Default, s.Nullable = "string", nil, true
			if c := codes(allowed); len(c) > 0 && strings.HasPrefix(c[0], "nch_") {
				s.Pattern = `^nch_[0-9A-HJKMNP-TV-Z]{26}$`
			}
		case d == "[]":
			s.Type, s.Default, s.Items = "array", []string{}, "string"
			s.Pattern = `^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`
		case strings.HasPrefix(d, `"`):
			s.Type, s.Default = "string", strings.Trim(d, `"`)
			if c := codes(allowed); len(c) > 0 {
				s.Pattern = c[0]
			}
		default:
			var n int64
			if _, err := fmt.Sscanf(d, "%d", &n); err != nil {
				return nil, fmt.Errorf("§6.13 default %q of %s", d, s.Key)
			}
			s.Type, s.Default = "integer", n
			m := rangeRE.FindStringSubmatch(allowed)
			if m == nil {
				return nil, fmt.Errorf("§6.13 range %q of %s", allowed, s.Key)
			}
			var lo, hi int64
			_, _ = fmt.Sscanf(m[1], "%d", &lo)
			_, _ = fmt.Sscanf(m[2], "%d", &hi)
			s.Min, s.Max = &lo, &hi
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("§6.13 has no rows")
	}
	return out, nil
}
