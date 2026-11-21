package archtest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/stainedhead/enterprise-agent-factory-service-restapi"

type pkg struct {
	ImportPath string
	Imports    []string
	Deps       []string
	Standard   bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// goList runs go list -json with args and decodes the stream of packages.
func goList(t *testing.T, args ...string) []pkg {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list", "-json=ImportPath,Imports,Deps,Standard"}, args...)...) //nolint:gosec // test helper; args are package patterns written in this file
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func standard(t *testing.T) map[string]bool {
	t.Helper()
	std := map[string]bool{}
	for _, p := range goList(t, "std") {
		std[p.ImportPath] = true
	}
	return std
}

// TestDependencyRule: domain and usecase import only the standard library, authz and each other.
func TestDependencyRule(t *testing.T) {
	std := standard(t)
	allowed := []string{module + "/internal/domain", module + "/internal/usecase", module + "/internal/authz"}
	for _, p := range goList(t, "./internal/domain/...", "./internal/usecase/...") {
		for _, imp := range p.Imports {
			if std[imp] || hasAnyPrefix(imp, allowed) {
				continue
			}
			t.Errorf("%s imports %s: domain and usecase may import only the standard library, authz, domain and usecase", p.ImportPath, imp)
		}
	}
	for _, p := range goList(t, "./internal/authz/...") {
		for _, imp := range p.Imports {
			if !std[imp] {
				t.Errorf("%s imports %s: authz imports only the standard library", p.ImportPath, imp)
			}
		}
	}
}

// TestAPIBinaryClients: afs-api constructs no AWS service client except Secrets Manager (and the
// local RDS IAM token signer), no Kubernetes client and no S3 client (RA-US-5.4, RA-US-2b.4, RA-US-14.5).
func TestAPIBinaryClients(t *testing.T) {
	ps := goList(t, "./cmd/afs-api")
	if len(ps) != 1 {
		t.Fatalf("go list returned %d packages", len(ps))
	}
	for _, dep := range ps[0].Deps {
		if reason := forbiddenForAPI(dep); reason != "" {
			t.Errorf("afs-api depends on %s: %s", dep, reason)
		}
	}
}

func forbiddenForAPI(dep string) string {
	switch {
	case strings.HasPrefix(dep, "github.com/aws/aws-sdk-go-v2/service/"):
		svc := strings.SplitN(strings.TrimPrefix(dep, "github.com/aws/aws-sdk-go-v2/service/"), "/", 2)[0]
		if svc == "secretsmanager" || svc == "internal" {
			return ""
		}
		return "the only AWS service client in afs-api is Secrets Manager"
	case strings.HasPrefix(dep, "k8s.io/client-go"), strings.HasPrefix(dep, "sigs.k8s.io/controller-runtime"):
		return "afs-api never calls Kubernetes"
	}
	return ""
}

func TestForbiddenForAPI(t *testing.T) {
	cases := map[string]bool{
		"github.com/aws/aws-sdk-go-v2/service/secretsmanager":         false,
		"github.com/aws/aws-sdk-go-v2/service/internal/presigned-url": false,
		"github.com/aws/aws-sdk-go-v2/feature/rds/auth":               false,
		"github.com/aws/aws-sdk-go-v2/service/s3":                     true,
		"github.com/aws/aws-sdk-go-v2/service/iam":                    true,
		"k8s.io/client-go/kubernetes":                                 true,
		"sigs.k8s.io/controller-runtime/pkg/client":                   true,
		"net/http": false,
	}
	for dep, want := range cases {
		if got := forbiddenForAPI(dep) != ""; got != want {
			t.Errorf("forbiddenForAPI(%s) = %v, want %v", dep, got, want)
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if s == p || strings.HasPrefix(s, p+"/") {
			return true
		}
	}
	return false
}
