package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type fakeCluster struct {
	issuer string
	tokens map[string]auth.ClusterToken
	err    error
}

func (f fakeCluster) Issuer() string { return f.issuer }
func (f fakeCluster) Verify(_ context.Context, token string) (auth.ClusterToken, error) {
	if f.err != nil {
		return auth.ClusterToken{}, f.err
	}
	ct, ok := f.tokens[token]
	if !ok {
		return auth.ClusterToken{}, domain.Unauthenticated()
	}
	return ct, nil
}

// jwt builds an unsigned three-part token whose payload has the issuer; the fake verifier decides.
func jwt(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{"iss":"`+iss+`"}`)) + ".sig"
}

const (
	cluster = "https://oidc.eks.us-east-1.example.com/id/EXAMPLE"
	nsA     = "afs-t-payments"
)

func workloadRig() (*auth.Service, *auth.WorkloadAuth) {
	poolTok, agentTok, strayTok := jwt(cluster)+"1", jwt(cluster)+"2", jwt(cluster)+"3"
	w := &auth.WorkloadAuth{
		Verifier: fakeCluster{issuer: cluster + "/", tokens: map[string]auth.ClusterToken{
			poolTok:  {Namespace: nsA, ServiceAccount: "pool-triage", Pod: "pool-triage-abc-xyz"},
			agentTok: {Namespace: nsA, ServiceAccount: "agent-writer", Pod: "agent-writer-0"},
			strayTok: {Namespace: "kube-system", ServiceAccount: "coredns"},
		}},
		Lookup: &fakes.Credentials{Workloads: map[[2]string]port.WorkloadRecord{
			{nsA, "pool-triage"}:  {TeamID: "team_a", PoolID: "pool_1"},
			{nsA, "agent-writer"}: {TeamID: "team_a", AgentID: "agt_1"},
		}},
	}
	return &auth.Service{Workload: w, Providers: staticProviders{}, Verifier: refuseAll{}, Keys: &fakes.Credentials{}}, w
}

// RA-US-2b.1 (principal), RA-US-3a.1 (part): the principal of a pool carries the pool id, its team and
// the worker pod; an agent's carries the agent id and no pod.
func TestWorkloadPrincipal(t *testing.T) {
	svc, _ := workloadRig()
	p, err := svc.Authenticate(t.Context(), jwt(cluster)+"1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != authz.PrincipalWorkload || p.ID != "pool_1" || p.ServiceAdmin || p.SecurityOperator || len(p.Bindings) != 0 ||
		p.Workload == nil || *p.Workload != (authz.Workload{TeamID: "team_a", PoolID: "pool_1", WorkerPod: "pool-triage-abc-xyz"}) {
		t.Fatalf("pool principal: %+v %+v", p, p.Workload)
	}
	p, err = svc.Authenticate(t.Context(), jwt(cluster)+"2")
	if err != nil || p.ID != "agt_1" || *p.Workload != (authz.Workload{TeamID: "team_a", AgentID: "agt_1"}) {
		t.Fatalf("agent principal: %+v %v", p, err)
	}
}

func TestWorkloadRefusals(t *testing.T) {
	svc, w := workloadRig()
	for name, tc := range map[string]struct {
		token string
		want  domain.ErrorKind
	}{
		"a service account that is no agent or pool": {jwt(cluster) + "3", domain.KindUnauthenticated},
		"a token the verifier refuses":               {jwt(cluster) + "9", domain.KindUnauthenticated},
		"another cluster's issuer":                   {jwt("https://other.example.com"), domain.KindUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Authenticate(t.Context(), tc.token); domain.KindOf(err) != tc.want {
				t.Fatalf("got %v", err)
			}
		})
	}
	// A dependency that is down is 503, not 401.
	w.Verifier = fakeCluster{issuer: cluster, err: domain.Unavailable("cluster keys", errors.New("down"))}
	if _, err := svc.Authenticate(t.Context(), jwt(cluster)); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("keys down: %v", err)
	}
}

func TestWorkloadMatches(t *testing.T) {
	_, w := workloadRig()
	for token, want := range map[string]bool{
		jwt(cluster):                     true,
		jwt(cluster + "/"):               true,
		jwt("https://other.example.com"): false,
		jwt(""):                          false,
		"abc":                            false,
		"a.b.c":                          false,
		"a." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".c": false,
		"": false,
	} {
		if got := w.Matches(token); got != want {
			t.Errorf("Matches(%.30q) = %v, want %v", token, got, want)
		}
	}
}

// Without a workload authenticator a cluster token is just an unknown identity provider token.
func TestWithoutWorkloadAuthenticationTheTokenIsRefused(t *testing.T) {
	svc := &auth.Service{Providers: staticProviders{}, Verifier: refuseAll{}, Keys: &fakes.Credentials{}}
	if _, err := svc.Authenticate(t.Context(), jwt(cluster)); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("got %v", err)
	}
}

type staticProviders struct{}

func (staticProviders) EnabledProviders(context.Context) ([]auth.Provider, error) { return nil, nil }

type refuseAll struct{}

func (refuseAll) Verify(context.Context, string, []auth.Provider) (auth.Identity, error) {
	return auth.Identity{}, domain.Unauthenticated()
}

// The database session of a workload sees its own team only, and audit rows name the pool and the pod.
func TestSessionOfWorkload(t *testing.T) {
	pool := authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: "team_a", PoolID: "pool_1", WorkerPod: "pool-triage-abc-xyz"}}
	s := auth.SessionOf(pool, "req-1")
	if s.Actor != "pool_1" || s.ActorType != port.ActorPool || len(s.TeamIDs) != 1 || s.TeamIDs[0] != "team_a" || s.ServiceAdmin || s.SecurityOperator ||
		s.PoolID != "pool_1" || s.WorkerPod != "pool-triage-abc-xyz" {
		t.Fatalf("pool session: %+v", s)
	}
	agent := authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: "team_a", AgentID: "agt_1"}}
	if s = auth.SessionOf(agent, "req-2"); s.ActorType != port.ActorAgent || s.TeamIDs[0] != "team_a" || s.PoolID != "" || s.WorkerPod != "" {
		t.Fatalf("agent session: %+v", s)
	}
}
