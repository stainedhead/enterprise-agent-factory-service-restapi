package matrix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// Caller is a kind of caller of the matrix (API contract §2).
type Caller string

// Callers. Anonymous sends no credential. Workload needs workload authentication (M6-D2) and is
// skipped until the World has a token for it.
const (
	Anonymous        Caller = "anonymous"
	Viewer           Caller = "viewer"
	TeamAdmin        Caller = "team-admin"
	ServiceAdmin     Caller = "service-admin"
	SecurityOperator Caller = "security-operator"
	TeamKeyViewer    Caller = "team-key-viewer"
	TeamKeyAdmin     Caller = "team-key-admin"
	ServiceKey       Caller = "service-key"
	Workload         Caller = "workload"
	Unbound          Caller = "unbound-user"
)

// AllCallers lists every caller kind.
var AllCallers = []Caller{Anonymous, Viewer, TeamAdmin, ServiceAdmin, SecurityOperator, TeamKeyViewer, TeamKeyAdmin, ServiceKey, Workload, Unbound}

// Outcome is what a caller must get.
type Outcome int

// Outcomes.
const (
	Allowed      Outcome = iota // 2xx
	Unauthorized                // 401
	NotFound                    // 404
	Forbidden                   // 403
)

func (o Outcome) String() string {
	return [...]string{"2xx", "401", "404", "403"}[o]
}

// Operation is one row of api/contract/operations.json.
type Operation struct {
	OperationID string   `json:"operationId"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Scope       string   `json:"scope"`
	Permission  string   `json:"permission"`
	AnyOf       []string `json:"anyOf"`
	Condition   string   `json:"condition"`
}

func (o Operation) perms() []string {
	if len(o.AnyOf) > 0 {
		return o.AnyOf
	}
	if o.Permission != "" {
		return []string{o.Permission}
	}
	return nil
}

type grant struct {
	Role      string `json:"role"`
	Condition string `json:"condition"`
}

// Contract is the machine-readable permission catalog and operation list.
type Contract struct {
	Operations map[string]Operation
	grants     map[string][]grant
}

// RepoRoot returns the root of the -restapi repository.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// LoadContract reads api/contract/{operations,permissions}.json.
func LoadContract() (*Contract, error) {
	dir := filepath.Join(RepoRoot(), "api", "contract")
	var ops []Operation
	var perms struct {
		Permissions []struct {
			Name   string  `json:"name"`
			Grants []grant `json:"grants"`
		} `json:"permissions"`
	}
	for file, dst := range map[string]any{"operations.json": &ops, "permissions.json": &perms} {
		raw, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // fixed contract file names under the repo checkout
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
	c := &Contract{Operations: map[string]Operation{}, grants: map[string][]grant{}}
	for _, o := range ops {
		c.Operations[o.OperationID] = o
	}
	for _, p := range perms.Permissions {
		c.grants[p.Name] = p.Grants
	}
	return c, nil
}

// roleOf maps a caller to the role it holds on team A, if any.
func roleOf(c Caller) string {
	switch c {
	case Viewer, TeamKeyViewer:
		return "team_viewer"
	case TeamAdmin, TeamKeyAdmin:
		return "team_admin"
	}
	return ""
}

func (c *Contract) roleHolds(role string, perms []string, flags authz.TeamFlags) bool {
	for _, p := range perms {
		for _, g := range c.grants[p] {
			if g.Role == role && conditionHolds(g.Condition, flags) {
				return true
			}
		}
	}
	return false
}

// conditionHolds evaluates a grant condition for a request to team A. The relational work order
// conditions are decided by the use case once the work order is loaded; at route level they hold.
func conditionHolds(cond string, f authz.TeamFlags) bool {
	switch cond {
	case "":
		return true
	case "mayManageAwsAccounts":
		return f.MayManageAwsAccounts
	case "viewerKubectl":
		return f.ViewerKubectl
	case "mayAddExternalSystems":
		return f.MayAddExternalSystems
	}
	return true // assignedOrOwnRepos, assignedTeam
}

// Expected is the oracle: the outcome the PRD §3 rule gives caller for op on team A with flags.
func (c *Contract) Expected(op Operation, caller Caller, flags authz.TeamFlags) Outcome {
	if op.Scope == "public" {
		return Allowed
	}
	if caller == Anonymous {
		return Unauthorized
	}
	perms := op.perms()
	secop := caller == SecurityOperator || caller == ServiceKey
	switch op.Scope {
	case "team":
		switch {
		case caller == ServiceAdmin:
			return Allowed
		case secop || caller == Workload:
			return Forbidden
		case caller == Unbound:
			return NotFound
		}
		if op.Condition == "mayAddExternalSystems" && !flags.MayAddExternalSystems {
			return Forbidden
		}
		if c.roleHolds(roleOf(caller), perms, flags) {
			return Allowed
		}
		return Forbidden
	case "service":
		switch {
		case caller == ServiceAdmin:
			return Allowed
		case secop && c.roleHolds("security_operator", perms, flags):
			return Allowed
		}
		return Forbidden
	}
	// any: open to any signed-in caller that holds the permission somewhere (or needs none).
	if len(perms) == 0 || caller == ServiceAdmin {
		return Allowed
	}
	switch {
	case secop:
		if c.roleHolds("security_operator", perms, flags) {
			return Allowed
		}
	case caller == Workload:
		if slices.Contains(perms, string(authz.WorkOrdersReport)) {
			return Allowed
		}
	case caller == Unbound:
	default:
		if c.roleHolds(roleOf(caller), perms, flags) {
			return Allowed
		}
	}
	return Forbidden
}
