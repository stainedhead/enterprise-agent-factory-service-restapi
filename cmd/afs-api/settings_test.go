package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-8 (FR-R11 AC1): one table test per variable that moved into settings.
func TestSettingsClusterAndAccount(t *testing.T) {
	type check func(settings) bool
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		ok      check
	}{
		{"all unset", nil, false, func(s settings) bool {
			return s.AWSAccountID == "" && s.ClusterName == "" && s.ClusterEndpoint == "" && s.ClusterCAData == "" && s.Addr == ":8080"
		}},
		{"account id", map[string]string{"AFS_AWS_ACCOUNT_ID": "111122223333"}, false, func(s settings) bool { return s.AWSAccountID == "111122223333" }},
		{"account id too short", map[string]string{"AFS_AWS_ACCOUNT_ID": "1111"}, true, nil},
		{"account id not digits", map[string]string{"AFS_AWS_ACCOUNT_ID": "11112222333x"}, true, nil},
		{"cluster name", map[string]string{"AFS_CLUSTER_NAME": "afs-dev"}, false, func(s settings) bool { return s.ClusterName == "afs-dev" }},
		{"endpoint", map[string]string{"AFS_CLUSTER_ENDPOINT": "https://EXAMPLE.gr7.us-east-1.eks.amazonaws.com"}, false,
			func(s settings) bool { return s.ClusterEndpoint == "https://EXAMPLE.gr7.us-east-1.eks.amazonaws.com" }},
		{"endpoint not https", map[string]string{"AFS_CLUSTER_ENDPOINT": "http://example.internal"}, true, nil},
		{"endpoint not a URL", map[string]string{"AFS_CLUSTER_ENDPOINT": "::bad::"}, true, nil},
		{"ca data", map[string]string{"AFS_CLUSTER_CA_DATA": "Q0FEQVRB"}, false, func(s settings) bool { return s.ClusterCAData == "Q0FEQVRB" }},
		{"ca data not base64", map[string]string{"AFS_CLUSTER_CA_DATA": "not base64 !"}, true, nil},
		{"addr", map[string]string{"AFS_API_ADDR": ":9999"}, false, func(s settings) bool { return s.Addr == ":9999" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := settingsFrom(env(c.env))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && !c.ok(s) {
				t.Fatalf("settings = %+v", s)
			}
		})
	}
}

// D-8 (FR-R11 AC3): the environment is read in main.go and settingsFrom only, so a bad value is reported at
// startup. Any os.Getenv, os.LookupEnv or os.Environ elsewhere in the package's sources fails this test.
func TestEnvironmentIsReadOnlyInMainAndSettingsFrom(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "main.go" {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // names come from Glob("*.go") in the package directory
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if isFn && fn.Name.Name == "settingsFrom" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if ok && pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv" || sel.Sel.Name == "Environ") {
					t.Errorf("%s: os.%s outside main.go and settingsFrom; read it in settingsFrom", fset.Position(sel.Pos()), sel.Sel.Name)
				}
				return true
			})
		}
	}
	if checked < 10 {
		t.Fatalf("only %d source files checked; is the test run from cmd/afs-api?", checked)
	}
}
