// Command contractgen extracts the machine-readable parts of the root API contract (permissions §3,
// operations §4, errors §6, events §7) and the root PRD's service settings (§6.13) into
// api/contract/*.json, and generates the Go tables built from them: internal/authz/catalog_gen.go,
// internal/adapter/httpapi/routes_gen.go, internal/domain/events_gen.go and
// internal/domain/settings_gen.go. With -check it fails when a committed file differs.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ContractVersion is the API contract version the extract is built from.
const contractVersion = "1.3.1"

const (
	contractPath = "docs/contracts/api-contract.md"
	prdPath      = "enterprise-agent-factory-service-initial-PRD.md"
)

func main() {
	root := flag.String("root", "../enterprise-agent-factory-service", "root repository checkout")
	out := flag.String("out", ".", "repository root to write into")
	check := flag.Bool("check", false, "fail when a committed file differs instead of writing")
	skipMissing := flag.Bool("skip-missing", false, "with -check: succeed when the root checkout is absent")
	flag.Parse()
	if *check && *skipMissing {
		if _, err := os.Stat(filepath.Join(*root, contractPath)); errors.Is(err, fs.ErrNotExist) {
			fmt.Println("contractgen: root checkout not present, check skipped")
			return
		}
	}
	files, err := generate(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(1)
	}
	if *check {
		if diffs := compare(*out, files); len(diffs) > 0 {
			fmt.Fprintln(os.Stderr, "contractgen: generated files differ; run make contract:\n  "+strings.Join(diffs, "\n  "))
			os.Exit(1)
		}
		return
	}
	for name, data := range files {
		p := filepath.Join(*out, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			fmt.Fprintln(os.Stderr, "contractgen:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "contractgen:", err)
			os.Exit(1)
		}
	}
}

// Extract is everything parsed from the sources.
type Extract struct {
	Operations  []Operation
	Permissions []PermissionEntry
	Errors      []ErrorEntry
	Events      []EventEntry
	Settings    []SettingEntry
	Source      string
}

// generate parses the sources under root and returns the generated files by repository path.
func generate(root string) (map[string][]byte, error) {
	contract, err := os.ReadFile(filepath.Join(root, contractPath)) //nolint:gosec // developer tool
	if err != nil {
		return nil, err
	}
	prd, err := os.ReadFile(filepath.Join(root, prdPath)) //nolint:gosec // developer tool
	if err != nil {
		return nil, err
	}
	ex, err := extract(contract, prd)
	if err != nil {
		return nil, err
	}
	return render(ex)
}

var versionRE = regexp.MustCompile(`Contract v(\d+\.\d+\.\d+)`)

// extract parses both sources.
func extract(contract, prd []byte) (*Extract, error) {
	if m := versionRE.FindSubmatch(contract); m == nil || string(m[1]) != contractVersion {
		return nil, fmt.Errorf("contract version is not %s", contractVersion)
	}
	cl := strings.Split(string(contract), "\n")
	pl := strings.Split(string(prd), "\n")
	ex := &Extract{}
	var err error
	if ex.Permissions, err = parsePermissions(cl); err != nil {
		return nil, err
	}
	if ex.Operations, err = parseOperations(cl); err != nil {
		return nil, err
	}
	if ex.Errors, err = parseErrors(cl); err != nil {
		return nil, err
	}
	if ex.Events, err = parseEvents(cl); err != nil {
		return nil, err
	}
	if ex.Settings, err = parseSettings(pl); err != nil {
		return nil, err
	}
	ex.Settings = append(ex.Settings, collabSettings()...)
	if err := validate(ex); err != nil {
		return nil, err
	}
	ex.Source = fmt.Sprintf("contract %s\n%s  %s\n%s  %s (§6.13)\n", contractVersion,
		sum(contract), contractPath, sum(prd), prdPath)
	return ex, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// validate checks the extract for internal consistency.
func validate(ex *Extract) error {
	perms := map[string]bool{}
	for _, p := range ex.Permissions {
		if perms[p.Name] {
			return fmt.Errorf("permission %s listed twice", p.Name)
		}
		perms[p.Name] = true
	}
	ids := map[string]bool{}
	routes := map[string]string{}
	for _, op := range ex.Operations {
		if ids[op.OperationID] {
			return fmt.Errorf("operation %s listed twice", op.OperationID)
		}
		ids[op.OperationID] = true
		key := op.Method + " " + op.Path
		if prev, ok := routes[key]; ok {
			return fmt.Errorf("%s and %s share %s", prev, op.OperationID, key)
		}
		routes[key] = op.OperationID
		for _, p := range append(append([]string{op.Permission}, op.AnyOf...), values(op.FieldPermissions)...) {
			if p != "" && !perms[p] {
				return fmt.Errorf("%s needs unknown permission %s", op.OperationID, p)
			}
		}
		if op.Permission == "" && op.Scope != "public" && op.Scope != "any" {
			return fmt.Errorf("%s has no permission", op.OperationID)
		}
	}
	return nil
}

func values(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// render produces every generated file.
func render(ex *Extract) (map[string][]byte, error) {
	files := map[string][]byte{}
	for name, v := range map[string]any{
		"api/contract/operations.json":  ex.Operations,
		"api/contract/permissions.json": permissionsDoc(ex.Permissions),
		"api/contract/errors.json":      ex.Errors,
		"api/contract/events.json":      ex.Events,
		"api/contract/settings.json":    ex.Settings,
	} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		files[name] = append(b, '\n')
	}
	files["api/contract/SOURCE"] = []byte(ex.Source)
	for name, gen := range map[string]func(*Extract) []byte{
		"internal/authz/catalog_gen.go":          genCatalog,
		"internal/adapter/httpapi/routes_gen.go": genRoutes,
		"internal/domain/events_gen.go":          genEvents,
		"internal/domain/settings_gen.go":        genSettings,
	} {
		src, err := format.Source(gen(ex))
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", name, err)
		}
		files[name] = src
	}
	return files, nil
}

// workloadGrant is API contract §2: a workload principal holds only work-orders:report on work
// orders assigned to its own team.
var workloadGrant = PermissionEntry{Name: "work-orders:report", Grants: []Grant{{Role: "workload", Condition: "assignedTeam"}}}

type permissionsFile struct {
	Roles       []string          `json:"roles"`
	Permissions []PermissionEntry `json:"permissions"`
	Workload    PermissionEntry   `json:"workload"`
}

func permissionsDoc(ps []PermissionEntry) permissionsFile {
	return permissionsFile{Roles: roles, Permissions: ps, Workload: workloadGrant}
}

// compare returns the names of files whose committed content differs.
func compare(dir string, files map[string][]byte) []string {
	var diffs []string
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // developer tool
		if err != nil || !bytes.Equal(got, want) {
			diffs = append(diffs, name)
		}
	}
	return diffs
}
