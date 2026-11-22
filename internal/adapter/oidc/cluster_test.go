package oidc

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func clusterSetup(t *testing.T) (*idp, *ClusterVerifier, *clock) {
	t.Helper()
	p := newIDP(t)
	p.addRSA(t, "k1")
	ck := &clock{t: t0}
	return p, NewClusterVerifier(p.srv.URL, Options{HTTP: p.srv.Client(), Now: ck.now}), ck
}

func saToken(p *idp, extra map[string]any) map[string]any {
	c := map[string]any{
		"iss": p.srv.URL, "sub": "system:serviceaccount:afs-t-payments:pool-triage", "aud": []string{"afs-api"},
		"iat": t0.Unix(), "exp": t0.Add(time.Hour).Unix(),
		"kubernetes.io": map[string]any{
			"namespace":      "afs-t-payments",
			"serviceaccount": map[string]any{"name": "pool-triage", "uid": "abc"},
			"pod":            map[string]any{"name": "pool-triage-7d9f8-x2k4q", "uid": "def"},
		},
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

// RA-US-2b.4: a valid projected token is accepted from the issuer's keys alone.
func TestClusterVerifyAccepts(t *testing.T) {
	p, v, _ := clusterSetup(t)
	ct, err := v.Verify(t.Context(), p.sign(t, "k1", saToken(p, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if ct.Namespace != "afs-t-payments" || ct.ServiceAccount != "pool-triage" || ct.Pod != "pool-triage-7d9f8-x2k4q" {
		t.Fatalf("%+v", ct)
	}
	// Audience as a plain string, no kubernetes.io claim, no iat.
	tok := p.sign(t, "k1", saToken(p, map[string]any{"aud": "afs-api", "kubernetes.io": nil, "iat": nil}))
	if ct, err = v.Verify(t.Context(), tok); err != nil || ct.ServiceAccount != "pool-triage" || ct.Pod != "" {
		t.Fatalf("%+v %v", ct, err)
	}
}

// RA-US-2b.3 and the expiry rule of RA-2b.
func TestClusterVerifyRefuses(t *testing.T) {
	p, v, _ := clusterSetup(t)
	other := newIDP(t)
	other.addRSA(t, "k1")
	for name, tc := range map[string]struct {
		token string
		want  domain.ErrorKind
	}{
		"gateway audience":         {p.sign(t, "k1", saToken(p, map[string]any{"aud": []string{"afs-gateway"}})), domain.KindUnauthenticated},
		"api and gateway audience": {p.sign(t, "k1", saToken(p, map[string]any{"aud": []string{"afs-api", "afs-gateway"}})), domain.KindUnauthenticated},
		"api server audience":      {p.sign(t, "k1", saToken(p, map[string]any{"aud": []string{"https://kubernetes.default.svc"}})), domain.KindUnauthenticated},
		"no audience":              {p.sign(t, "k1", saToken(p, map[string]any{"aud": nil})), domain.KindUnauthenticated},
		"expired":                  {p.sign(t, "k1", saToken(p, map[string]any{"iat": t0.Add(-2 * time.Hour).Unix(), "exp": t0.Add(-time.Hour).Unix()})), domain.KindUnauthenticated},
		"no expiry":                {p.sign(t, "k1", saToken(p, map[string]any{"exp": nil})), domain.KindUnauthenticated},
		"expires in two hours":     {p.sign(t, "k1", saToken(p, map[string]any{"iat": nil, "exp": t0.Add(2 * time.Hour).Unix()})), domain.KindUnauthenticated},
		"issued for a year":        {p.sign(t, "k1", saToken(p, map[string]any{"iat": t0.Add(-364 * 24 * time.Hour).Unix(), "exp": t0.Add(time.Minute).Unix()})), domain.KindUnauthenticated},
		"not yet valid":            {p.sign(t, "k1", saToken(p, map[string]any{"nbf": t0.Add(time.Hour).Unix()})), domain.KindUnauthenticated},
		"another cluster":          {other.sign(t, "k1", saToken(other, nil)), domain.KindUnauthenticated},
		"another cluster's keys":   {other.sign(t, "k1", saToken(p, nil)), domain.KindUnauthenticated},
		"unknown key id":           {p.sign(t, "k1", saToken(p, nil))[:20] + "x", domain.KindUnauthenticated},
		"subject is a user":        {p.sign(t, "k1", saToken(p, map[string]any{"sub": "alice"})), domain.KindUnauthenticated},
		"subject is a node":        {p.sign(t, "k1", saToken(p, map[string]any{"sub": "system:node:ip-10-0-0-1"})), domain.KindUnauthenticated},
		"claim disagrees":          {p.sign(t, "k1", saToken(p, map[string]any{"sub": "system:serviceaccount:afs-t-other:pool-triage"})), domain.KindUnauthenticated},
		"not a token":              {"abc", domain.KindUnauthenticated},
		"empty":                    {"", domain.KindUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			if ct, err := v.Verify(t.Context(), tc.token); domain.KindOf(err) != tc.want {
				t.Fatalf("got %+v %v, want %s", ct, err, tc.want)
			}
		})
	}
}

// An unsigned token and a symmetric one are refused (the verifier holds no shared secret).
func TestClusterVerifyRefusesNoneAndSymmetric(t *testing.T) {
	_, v, _ := clusterSetup(t)
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJpc3MiOiJ4In0."
	if _, err := v.Verify(t.Context(), none); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("alg none: %v", err)
	}
}

// RA-US-2b.4: validation is offline. Once the keys are cached the issuer may be unreachable (a stand-in
// for the Kubernetes API being down); with nothing cached the answer is 503, never 401.
func TestClusterVerifyWorksWhileTheIssuerIsDown(t *testing.T) {
	p, v, ck := clusterSetup(t)
	tok := p.sign(t, "k1", saToken(p, nil))
	if _, err := v.Verify(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	p.setDown(true)
	ck.t = t0.Add(20 * time.Minute) // past the key cache's ten minutes: a refetch is tried and fails
	tok = p.sign(t, "k1", saToken(p, map[string]any{"iat": ck.t.Unix(), "exp": ck.t.Add(time.Hour).Unix()}))
	if ct, err := v.Verify(t.Context(), tok); err != nil || ct.ServiceAccount != "pool-triage" {
		t.Fatalf("with cached keys: %+v %v", ct, err)
	}

	p2, cold, _ := clusterSetup(t)
	p2.setDown(true)
	if _, err := cold.Verify(t.Context(), p2.sign(t, "k1", saToken(p2, nil))); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("no keys cached and the issuer down: %v", err)
	}
}

func TestClusterVerifierTrimsTheIssuer(t *testing.T) {
	p, _, ck := clusterSetup(t)
	v := NewClusterVerifier(p.srv.URL+"/", Options{HTTP: p.srv.Client(), Now: ck.now})
	if v.Issuer() != p.srv.URL {
		t.Fatalf("issuer %q", v.Issuer())
	}
	if _, err := v.Verify(t.Context(), p.sign(t, "k1", saToken(p, map[string]any{"iss": p.srv.URL + "/"}))); err != nil {
		t.Fatal(err)
	}
}

func TestClusterVerifyDropsAnOddPodName(t *testing.T) {
	p, v, _ := clusterSetup(t)
	k8s := saToken(p, nil)["kubernetes.io"].(map[string]any)
	k8s["pod"] = map[string]any{"name": "Not_A_Pod!"}
	ct, err := v.Verify(t.Context(), p.sign(t, "k1", saToken(p, map[string]any{"kubernetes.io": k8s})))
	if err != nil || ct.Pod != "" {
		t.Fatalf("%+v %v", ct, err)
	}
}
