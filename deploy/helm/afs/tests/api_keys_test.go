package tests

import "testing"

// secretKeyRefOf returns the secretKeyRef of the named env var of the first container, and whether the
// variable carries an inline value.
func secretKeyRefOf(t *testing.T, docs []obj, deployment, env string) (ref obj, inline bool) {
	t.Helper()
	d := find(t, docs, "Deployment", deployment)
	for _, e := range podSpec(d)["containers"].([]any)[0].(obj)["env"].([]any) {
		m := e.(obj)
		if str(m, "name") != env {
			continue
		}
		_, inline = m["value"]
		return path(m, "valueFrom", "secretKeyRef"), inline
	}
	t.Fatalf("%s: env %s not rendered", deployment, env)
	return nil, false
}

// D-3 (FR-R07 AC1) and D-5 (RD-2): the page-token key and the idempotency HMAC key reach every afs-api
// replica from one Kubernetes Secret by secretKeyRef, never as an inline value.
func TestAPIKeysComeFromASecret(t *testing.T) {
	for _, tc := range []struct{ overrides, secret string }{
		{"", "afs-api-keys"},
		{"api:\n  keysSecret:\n    name: my-keys\n", "my-keys"},
	} {
		docs := render(t, tc.overrides)
		for env, key := range map[string]string{"AFS_PAGE_TOKEN_KEY": "pageTokenKey", "AFS_IDEMPOTENCY_HMAC_KEY": "idempotencyHmacKey"} {
			ref, inline := secretKeyRefOf(t, docs, "afs-api", env)
			if inline {
				t.Errorf("%s has an inline value", env)
			}
			if str(ref, "name") != tc.secret || str(ref, "key") != key {
				t.Errorf("%s secretKeyRef = %v, want %s/%s", env, ref, tc.secret, key)
			}
		}
		if got := deployEnv(t, docs, "afs-api")["AFS_API_REPLICAS"]; got != "3" {
			t.Errorf("AFS_API_REPLICAS = %q, want 3", got)
		}
		for _, other := range []string{"afs-reconciler", "afs-gateway"} {
			env := deployEnv(t, docs, other)
			if _, ok := env["AFS_PAGE_TOKEN_KEY"]; ok {
				t.Errorf("%s receives AFS_PAGE_TOKEN_KEY", other)
			}
		}
	}
}

// RD-1: AFS_MAX_BODY_BYTES was never read by afs-api; the chart no longer sets it.
func TestAPIHasNoMaxBodyBytes(t *testing.T) {
	if _, ok := deployEnv(t, render(t, ""), "afs-api")["AFS_MAX_BODY_BYTES"]; ok {
		t.Error("AFS_MAX_BODY_BYTES is rendered, but afs-api does not read it")
	}
}
