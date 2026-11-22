package main

import "testing"

func TestSettingsFromPipelineVars(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		replicas, hp int
		wantErr      bool
	}{
		{"defaults", nil, 1, 0, false},
		{"set", map[string]string{"AFS_API_REPLICAS": "4", "AFS_TRUSTED_PROXY_HOPS": "2", "AFS_PAGE_TOKEN_KEY": "0123456789abcdef0123456789abcdef", "AFS_IDEMPOTENCY_HMAC_KEY": "fedcba9876543210fedcba9876543210"}, 4, 2, false},
		{"zero replicas", map[string]string{"AFS_API_REPLICAS": "0"}, 0, 0, true},
		{"negative hops", map[string]string{"AFS_TRUSTED_PROXY_HOPS": "-1"}, 0, 0, true},
		{"not a number", map[string]string{"AFS_API_REPLICAS": "many"}, 0, 0, true},
		{"too large", map[string]string{"AFS_API_REPLICAS": "100000"}, 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := settingsFrom(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && (s.APIReplicas != tc.replicas || s.TrustedProxyHops != tc.hp) {
				t.Fatalf("got replicas %d hops %d", s.APIReplicas, s.TrustedProxyHops)
			}
		})
	}
}
