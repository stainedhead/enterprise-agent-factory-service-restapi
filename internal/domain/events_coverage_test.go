package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryEventTypeIsEmittedAndTested is M6-F1 (CT-API-7): every API contract §7 event type, and every extension
// type, is emitted by production code (a reference to its constant, or its name in a non-test source) and named
// by at least one test. It scans the module's Go files, so a new event type without an emitter or a test fails.
func TestEveryEventTypeIsEmittedAndTested(t *testing.T) {
	consts := eventConstants(t, "events_gen.go", "events_ext.go")
	if len(consts) < 50 {
		t.Fatalf("found %d event constants: the scan is broken", len(consts))
	}
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var prod, tests []string
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "events_gen.go") || strings.HasSuffix(p, "events_ext.go") || strings.HasSuffix(p, "events_coverage_test.go") {
			return nil
		}
		b, err := fs.ReadFile(root.FS(), p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, "_test.go") {
			tests = append(tests, string(b))
		} else {
			prod = append(prod, string(b))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mentions := func(srcs []string, typ EventType, name string) bool {
		for _, s := range srcs {
			if strings.Contains(s, "domain."+name) || strings.Contains(s, "\""+string(typ)+"\"") || strings.Contains(s, "'"+string(typ)+"'") || strings.Contains(s, "`"+string(typ)+"`") {
				return true
			}
			// Inside package domain the constant is used unqualified.
			if strings.Contains(s, "package domain\n") && strings.Contains(s, name) {
				return true
			}
		}
		return false
	}
	var types []string
	for typ := range consts {
		types = append(types, string(typ))
	}
	sort.Strings(types)
	for _, ts := range types {
		typ, name := EventType(ts), consts[EventType(ts)]
		if !mentions(prod, typ, name) {
			t.Errorf("%s (%s) has no emitter in production code", typ, name)
		}
		if !mentions(tests, typ, name) {
			t.Errorf("%s (%s) is named by no test", typ, name)
		}
	}
	// Every constant is either a contract type or a listed extension: no constant escapes Known.
	for typ := range consts {
		if !typ.Known() {
			t.Errorf("%s is a constant but not a known event type", typ)
		}
	}
}

// eventConstants maps each EventType constant's value to its identifier.
func eventConstants(t *testing.T, files ...string) map[EventType]string {
	t.Helper()
	out := map[EventType]string{}
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "EventType" || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.BasicLit)
				if !ok {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[EventType(v)] = vs.Names[0].Name
			}
		}
	}
	return out
}
