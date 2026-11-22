package kubeconfig_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/kubeconfig"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var cluster = kubeconfig.Cluster{
	Name: "afs", Endpoint: "https://EXAMPLE0123456789.gr7.us-east-1.eks.amazonaws.com",
	CAData: "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCkVYQU1QTEUKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo=", Region: "us-east-1",
}

type repo struct {
	teams map[string]kubeconfig.Team
	err   error
}

func (r repo) Team(_ context.Context, _ port.Tx, id string) (kubeconfig.Team, error) {
	if r.err != nil {
		return kubeconfig.Team{}, r.err
	}
	t, ok := r.teams[id]
	if !ok {
		return kubeconfig.Team{}, domain.NotFound("team")
	}
	return t, nil
}

func rig(c kubeconfig.Cluster, on bool) (*kubeconfig.Service, *fakes.Audit) {
	audit := &fakes.Audit{}
	return &kubeconfig.Service{
		Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{},
			Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}},
		Repo:    repo{teams: map[string]kubeconfig.Team{teamA: {Slug: "payments", Namespace: "afs-t-payments", ViewerKubectl: on}}},
		Cluster: c,
	}, audit
}

func bound(role authz.Role) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: teamA}}}}
}

func TestGetKubeconfigDocument(t *testing.T) {
	svc, audit := rig(cluster, true)
	out, err := svc.Get(t.Context(), bound(authz.RoleTeamViewer), teamA)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	if doc["kind"] != "Config" || doc["current-context"] != "afs-payments" {
		t.Errorf("document = %v", doc)
	}
	text := string(out)
	for _, want := range []string{"server: " + cluster.Endpoint, "namespace: afs-t-payments", "command: aws",
		"- eks", "- get-token", "- --cluster-name", "- afs", "- --region", "- us-east-1", "client.authentication.k8s.io/v1beta1"} {
		if !strings.Contains(text, want) {
			t.Errorf("document lacks %q:\n%s", want, text)
		}
	}
	// RA-US-12.1: a kubeconfig carries no credential, only the exec plugin that fetches one.
	for _, bad := range []string{"token:", "client-certificate", "client-key", "password", "Bearer", "auth-provider"} {
		if strings.Contains(text, bad) {
			t.Errorf("document contains %q:\n%s", bad, text)
		}
	}
	if len(audit.Entries) != 0 {
		t.Errorf("a read wrote audit records: %v", audit.Entries)
	}
}

func TestGetKubeconfigAuthorization(t *testing.T) {
	cases := []struct {
		name   string
		caller acting.Caller
		on     bool
		want   domain.ErrorKind // "" is success
		denied bool
	}{
		{"viewer, flag on", bound(authz.RoleTeamViewer), true, "", false},
		{"team admin, flag on", bound(authz.RoleTeamAdmin), true, "", false},
		{"service admin, flag on", acting.Caller{Principal: authz.Principal{ID: "u", ServiceAdmin: true}}, true, "", false},
		{"service admin, flag off", acting.Caller{Principal: authz.Principal{ID: "u", ServiceAdmin: true}}, false, "", false},
		{"viewer, flag off", bound(authz.RoleTeamViewer), false, domain.KindAccessDenied, true},
		{"team admin, flag off", bound(authz.RoleTeamAdmin), false, domain.KindAccessDenied, true},
		{"security operator", acting.Caller{Principal: authz.Principal{ID: "u", SecurityOperator: true}}, true, domain.KindAccessDenied, true},
		{"unbound user", acting.Caller{Principal: authz.Principal{ID: "u"}}, true, domain.KindNotFound, false},
		{"viewer of another team", acting.Caller{Principal: authz.Principal{ID: "u", Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamB}}}}, true, domain.KindNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, audit := rig(cluster, tc.on)
			out, err := svc.Get(t.Context(), tc.caller, teamA)
			if got := domain.KindOf(err); got != tc.want {
				t.Fatalf("kind = %q (%v), want %q", got, err, tc.want)
			}
			if tc.want == "" && len(out) == 0 {
				t.Error("no document")
			}
			if tc.want != "" && len(out) != 0 {
				t.Error("a refused request returned a document")
			}
			if got := len(audit.Entries) == 1; got != tc.denied {
				t.Errorf("denied audit = %v, want %v (%v)", audit.Entries, tc.denied, got)
			}
		})
	}
}

func TestGetKubeconfigUnknownTeamAndFailures(t *testing.T) {
	svc, _ := rig(cluster, true)
	admin := acting.Caller{Principal: authz.Principal{ID: "u", ServiceAdmin: true}}
	if _, err := svc.Get(t.Context(), admin, teamB); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown team: %v", err)
	}
	svc.Repo = repo{err: errors.New("db down")}
	if _, err := svc.Get(t.Context(), admin, teamA); err == nil {
		t.Error("a repository failure was swallowed")
	}
}

func TestGetKubeconfigNeedsAConfiguredCluster(t *testing.T) {
	for name, c := range map[string]kubeconfig.Cluster{
		"no endpoint": {Name: "afs", CAData: cluster.CAData, Region: "us-east-1"},
		"no ca":       {Name: "afs", Endpoint: cluster.Endpoint, Region: "us-east-1"},
		"no name":     {Endpoint: cluster.Endpoint, CAData: cluster.CAData, Region: "us-east-1"},
		"no region":   {Name: "afs", Endpoint: cluster.Endpoint, CAData: cluster.CAData},
		"http":        {Name: "afs", Endpoint: "http://example.com", CAData: cluster.CAData, Region: "us-east-1"},
		"bad ca":      {Name: "afs", Endpoint: cluster.Endpoint, CAData: "not base64!", Region: "us-east-1"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := rig(c, true)
			_, err := svc.Get(t.Context(), bound(authz.RoleTeamViewer), teamA)
			if domain.KindOf(err) != domain.KindUnavailable {
				t.Fatalf("kind = %q (%v), want Unavailable", domain.KindOf(err), err)
			}
		})
	}
}

func TestRenderQuotesWhatYAMLWouldMisread(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"true", `- "true"`}, {"123", `- "123"`}, {"a: b", `- "a: b"`}, {"plain-name", `- plain-name`}, {"ends:", `- "ends:"`},
	} {
		doc, err := kubeconfig.Render(kubeconfig.Cluster{Name: tc.name, Endpoint: "https://example.test", CAData: "QUJD", Region: "us-east-1"}, kubeconfig.Team{Slug: "x", Namespace: "afs-t-x"})
		if err != nil || !strings.Contains(string(doc), tc.want+"\n") {
			t.Errorf("%q: want %s in\n%s (err %v)", tc.name, tc.want, doc, err)
		}
	}
}
