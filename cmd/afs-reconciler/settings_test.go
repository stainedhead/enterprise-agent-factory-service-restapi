package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func host() func() (string, error) { return func() (string, error) { return "pod-0", nil } }

func TestSettingsFrom(t *testing.T) {
	base := map[string]string{"AFS_DATABASE_URL": "postgres://x", "AFS_GATEWAY_ROLE_ARN": "arn:aws:iam::111122223333:role/gw", "AFS_RECONCILER_USERS": " a , b,,"}
	s, err := settingsFrom(envOf(base), host())
	if err != nil {
		t.Fatal(err)
	}
	if s.MetricsAddr != ":9090" || s.LeaseNamespace != "afs-system" || s.PodName != "pod-0" || len(s.ReconcilerUsers) != 2 || s.ReconcilerUsers[1] != "b" || s.GatewayName != "afs-gateway" {
		t.Fatalf("defaults wrong: %+v", s)
	}
	cases := map[string]struct {
		mut  func(map[string]string)
		want string
	}{
		"no db":    {func(m map[string]string) { delete(m, "AFS_DATABASE_URL") }, "AFS_DATABASE_URL"},
		"no gw":    {func(m map[string]string) { delete(m, "AFS_GATEWAY_ROLE_ARN") }, "AFS_GATEWAY_ROLE_ARN"},
		"no users": {func(m map[string]string) { delete(m, "AFS_RECONCILER_USERS") }, "AFS_RECONCILER_USERS"},
		"bad poll": {func(m map[string]string) { m["AFS_POLL_INTERVAL"] = "soon" }, "AFS_POLL_INTERVAL"},
		"neg":      {func(m map[string]string) { m["AFS_RESYNC_INTERVAL"] = "-1s" }, "AFS_RESYNC_INTERVAL"},
	}
	for name, tc := range cases {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		tc.mut(m)
		if _, err := settingsFrom(envOf(m), host()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %s", name, err, tc.want)
		}
	}
	if _, err := settingsFrom(envOf(base), func() (string, error) { return "", errors.New("no") }); err == nil {
		t.Error("without POD_NAME the hostname is the identity; its failure must be reported")
	}
	m := map[string]string{"POD_NAME": "p"}
	for k, v := range base {
		m[k] = v
	}
	m["AFS_POLL_INTERVAL"] = "5s"
	if s, err := settingsFrom(envOf(m), func() (string, error) { return "", errors.New("no") }); err != nil || s.PodName != "p" || s.PollInterval != 5*time.Second {
		t.Errorf("s=%+v err=%v", s, err)
	}
}
