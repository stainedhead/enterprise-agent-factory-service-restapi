// Package tests renders the afs chart with the Helm library (the same engine as `helm template`)
// and asserts the deployment invariants of DP-1 and the availability NFR. It is a separate Go
// module so Helm's dependency tree never enters the service module; run it with `make helm-test`.
package tests

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const chartDir = ".."

type obj = map[string]any

// render renders the chart with ci/default-values.yaml plus overrides and returns the documents of
// every template, sorted by kind and name.
func render(t *testing.T, overrides string) []obj {
	t.Helper()
	ch, err := loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]any{}
	for _, f := range []string{filepath.Join(chartDir, "ci", "default-values.yaml")} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &vals); err != nil {
			t.Fatal(err)
		}
	}
	if overrides != "" {
		over := map[string]any{}
		if err := yaml.Unmarshal([]byte(overrides), &over); err != nil {
			t.Fatal(err)
		}
		vals = chartutil.CoalesceTables(over, vals)
	}
	rv, err := chartutil.ToRenderValues(ch, vals, chartutil.ReleaseOptions{Name: "afs", Namespace: "afs-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	files, err := engine.Render(ch, rv)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var docs []obj
	for name, content := range files {
		if strings.HasSuffix(name, "_helpers.tpl") {
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(content))
		for {
			var d obj
			if err := dec.Decode(&d); err != nil {
				if errors.Is(err, os.ErrClosed) || err.Error() == "EOF" {
					break
				}
				t.Fatalf("%s is not valid YAML: %v\n%s", name, err, content)
			}
			if d != nil {
				docs = append(docs, d)
			}
		}
	}
	sort.Slice(docs, func(i, j int) bool { return key(docs[i]) < key(docs[j]) })
	return docs
}

func key(d obj) string {
	return str(d, "kind") + "/" + str(path(d, "metadata"), "name")
}

func path(d obj, keys ...string) obj {
	cur := d
	for _, k := range keys {
		next, _ := cur[k].(obj)
		if next == nil {
			return obj{}
		}
		cur = next
	}
	return cur
}

func str(d obj, k string) string { s, _ := d[k].(string); return s }

func find(t *testing.T, docs []obj, kind, name string) obj {
	t.Helper()
	for _, d := range docs {
		if str(d, "kind") == kind && str(path(d, "metadata"), "name") == name {
			return d
		}
	}
	t.Fatalf("%s %s not rendered; have %v", kind, name, keys(docs))
	return nil
}

func keys(docs []obj) []string {
	var out []string
	for _, d := range docs {
		out = append(out, key(d))
	}
	return out
}

func podSpec(d obj) obj { return path(d, "spec", "template", "spec") }

func TestAvailabilityAndSpread(t *testing.T) {
	docs := render(t, "")
	for _, name := range []string{"afs-api", "afs-gateway", "afs-edge"} {
		d := find(t, docs, "Deployment", name)
		if n, _ := path(d, "spec")["replicas"].(int); n < 3 {
			t.Errorf("%s replicas = %v, want >= 3", name, path(d, "spec")["replicas"])
		}
		tsc, _ := podSpec(d)["topologySpreadConstraints"].([]any)
		zone := false
		for _, c := range tsc {
			cm := c.(obj)
			if cm["topologyKey"] == "topology.kubernetes.io/zone" && cm["whenUnsatisfiable"] == "DoNotSchedule" {
				zone = true
			}
		}
		if !zone {
			t.Errorf("%s is not spread across zones: %v", name, tsc)
		}
		pdb := find(t, docs, "PodDisruptionBudget", name)
		if path(pdb, "spec")["minAvailable"] != 2 {
			t.Errorf("%s PDB = %v", name, path(pdb, "spec"))
		}
	}
	rec := find(t, docs, "Deployment", "afs-reconciler")
	if n, _ := path(rec, "spec")["replicas"].(int); n < 2 {
		t.Errorf("reconciler has no standby: replicas = %v", path(rec, "spec")["replicas"])
	}
}

func TestContainersAreHardenedAndProbed(t *testing.T) {
	docs := render(t, "")
	for _, name := range []string{"afs-api", "afs-gateway", "afs-reconciler", "afs-edge"} {
		ps := podSpec(find(t, docs, "Deployment", name))
		if path(ps, "securityContext")["runAsNonRoot"] != true {
			t.Errorf("%s does not run as non-root", name)
		}
		c := ps["containers"].([]any)[0].(obj)
		sc := path(c, "securityContext")
		if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
			t.Errorf("%s container security context = %v", name, sc)
		}
		for _, probe := range []string{"livenessProbe", "readinessProbe"} {
			if c[probe] == nil {
				t.Errorf("%s lacks %s", name, probe)
			}
		}
		if path(path(c, "readinessProbe"), "httpGet")["path"] != "/readyz" || path(path(c, "livenessProbe"), "httpGet")["path"] != "/healthz" {
			t.Errorf("%s probes do not use /readyz and /healthz", name)
		}
		if !strings.HasPrefix(str(c, "image"), "111122223333.dkr.ecr.us-east-1.amazonaws.com/afs/afs-") || !strings.HasSuffix(str(c, "image"), ":v1.1.0") {
			t.Errorf("%s image = %q", name, str(c, "image"))
		}
	}
}

func TestEachBinaryHasItsOwnServiceAccountAndDatabaseRole(t *testing.T) {
	docs := render(t, "")
	want := map[string]string{"afs-api": "afs_app", "afs-gateway": "afs_gateway", "afs-reconciler": "afs_reconciler", "afs-edge": "afs_edge"}
	for dep, role := range want {
		find(t, docs, "ServiceAccount", dep)
		ps := podSpec(find(t, docs, "Deployment", dep))
		if ps["serviceAccountName"] != dep {
			t.Errorf("%s runs as %v", dep, ps["serviceAccountName"])
		}
		env := envOf(ps["containers"].([]any)[0].(obj))
		if !strings.HasPrefix(env["AFS_DATABASE_URL"], "postgres://"+role+"@") || strings.Contains(env["AFS_DATABASE_URL"], ":@") || strings.Contains(env["AFS_DATABASE_URL"], "password") {
			t.Errorf("%s database url = %q, want login %s and no password", dep, env["AFS_DATABASE_URL"], role)
		}
		if env["AFS_DB_IAM_AUTH"] != "true" || env["AFS_AWS_REGION"] != "us-east-1" || env["AFS_CLUSTER_NAME"] != "afs-dev" {
			t.Errorf("%s env = %v", dep, env)
		}
	}
	find(t, docs, "ServiceAccount", "afs-billing")
}

func envOf(c obj) map[string]string {
	out := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(obj)
		out[str(m, "name")], _ = m["value"].(string)
	}
	return out
}

func TestGatewayTLSFromCertManager(t *testing.T) {
	docs := render(t, "")
	cert := find(t, docs, "Certificate", "afs-gateway")
	spec := path(cert, "spec")
	if spec["secretName"] != "afs-gateway-tls" || path(spec, "issuerRef")["name"] != "afs-ca" {
		t.Errorf("certificate spec = %v", spec)
	}
	dns, _ := spec["dnsNames"].([]any)
	found := false
	for _, d := range dns {
		if d == "afs-gateway.afs-system.svc" {
			found = true
		}
	}
	if !found {
		t.Errorf("service DNS name missing: %v", dns)
	}
	ps := podSpec(find(t, docs, "Deployment", "afs-gateway"))
	vols, _ := ps["volumes"].([]any)
	if len(vols) != 1 || path(vols[0].(obj), "secret")["secretName"] != "afs-gateway-tls" {
		t.Errorf("gateway does not mount its certificate: %v", vols)
	}
	env := envOf(ps["containers"].([]any)[0].(obj))
	if env["AFS_GATEWAY_TLS_CERT"] != "/tls/tls.crt" || env["AFS_GATEWAY_TLS_KEY"] != "/tls/tls.key" {
		t.Errorf("tls env = %v", env)
	}
}

func TestBillingCronJobs(t *testing.T) {
	docs := render(t, "")
	n := 0
	for _, d := range docs {
		if str(d, "kind") != "CronJob" {
			continue
		}
		n++
		spec := path(d, "spec")
		if spec["concurrencyPolicy"] != "Forbid" || str(spec, "schedule") == "" {
			t.Errorf("%s spec = %v", key(d), spec)
		}
		c := path(spec, "jobTemplate", "spec", "template", "spec")["containers"].([]any)[0].(obj)
		args, _ := c["args"].([]any)
		if len(args) != 2 || args[0] != "--job" {
			t.Errorf("%s args = %v", key(d), args)
		}
	}
	if n != 10 { // 6 through 1.1.0, plus the four route billing jobs of 1.2.0
		t.Errorf("%d billing CronJobs, want 10", n)
	}
	for _, name := range []string{"cur-ingest", "route-pull", "route-statements", "route-capacity", "route-price-sync"} {
		if find(t, docs, "CronJob", "afs-billing-"+name) == nil {
			t.Fatalf("%s missing", name)
		}
	}
}

func TestServiceMonitorsOptional(t *testing.T) {
	if n := count(render(t, ""), "ServiceMonitor"); n != 4 {
		t.Errorf("ServiceMonitors = %d, want 4", n)
	}
	if n := count(render(t, "serviceMonitor:\n  enabled: false\n"), "ServiceMonitor"); n != 0 {
		t.Errorf("ServiceMonitors with the switch off = %d", n)
	}
}

func count(docs []obj, kind string) int {
	n := 0
	for _, d := range docs {
		if str(d, "kind") == kind {
			n++
		}
	}
	return n
}

func TestComponentOverridesMergeOverFiles(t *testing.T) {
	docs := render(t, "api:\n  replicas: 5\n  rateLimitReplicas: 4\ngateway:\n  enabled: false\n")
	api := find(t, docs, "Deployment", "afs-api")
	if path(api, "spec")["replicas"] != 5 {
		t.Errorf("replicas = %v", path(api, "spec")["replicas"])
	}
	if env := envOf(podSpec(api)["containers"].([]any)[0].(obj)); env["AFS_API_REPLICAS"] != "4" {
		t.Errorf("rate limit replicas = %q", env["AFS_API_REPLICAS"])
	}
	for _, d := range docs {
		if strings.Contains(key(d), "gateway") {
			t.Errorf("disabled gateway still renders %s", key(d))
		}
	}
}

func TestRequiredValuesFailFast(t *testing.T) {
	ch, err := loader.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	rv, err := chartutil.ToRenderValues(ch, map[string]any{}, chartutil.ReleaseOptions{Name: "afs", Namespace: "x"}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Render(ch, rv); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("chart rendered without registry, region, cluster or database host: %v", err)
	}
}

func TestNoSecretValuesInManifests(t *testing.T) {
	for _, d := range render(t, "") {
		b, _ := yaml.Marshal(d)
		s := strings.ToLower(string(b))
		for _, bad := range []string{"password:", "secretaccesskey", "aws_secret", "begin private key"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", key(d), bad)
			}
		}
	}
}

// TestGolden compares the rendered chart with testdata/default.golden.yaml (run with -update to
// rewrite it after an intended change).
func TestGolden(t *testing.T) {
	var buf bytes.Buffer
	for _, d := range render(t, "") {
		b, err := yaml.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		buf.WriteString("---\n")
		buf.Write(b)
	}
	golden := filepath.Join("testdata", "default.golden.yaml")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `go test -update` once to create it)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Error("rendered chart differs from testdata/default.golden.yaml; review the diff and run `go test -update`")
	}
}

func TestObservabilityResources(t *testing.T) {
	docs := render(t, "")
	rule := find(t, docs, "PrometheusRule", "afs-slis")
	groups, _ := path(rule, "spec")["groups"].([]any)
	alerts := map[string]bool{}
	for _, g := range groups {
		for _, r := range g.(obj)["rules"].([]any) {
			alerts[str(r.(obj), "alert")] = true
		}
	}
	// one alert per SLI of the NFR table
	for _, want := range []string{"AFSAPIReadLatencyHigh", "AFSAPIReportLatencyHigh", "AFSAPIErrorRatioHigh", "AFSReconcileLagHigh", "AFSDriftHigh",
		"AFSGatewayReservationFailures", "AFSGatewayAddedLatencyHigh", "AFSGatewayReservationsExpiring", "AFSOutboxLagHigh", "AFSNotificationFailures", "AFSCURVarianceHigh", "AFSNoReconcilerLeader", "AFSGatewaySettleFailures"} {
		if !alerts[want] {
			t.Errorf("alert %s missing", want)
		}
	}
	cm := find(t, docs, "ConfigMap", "afs-dashboards")
	data, _ := cm["data"].(obj)
	if !strings.Contains(str(data, "afs-slis.json"), `"uid": "afs-slis"`) {
		t.Errorf("dashboard not embedded: %v", data)
	}
	if n := count(render(t, "prometheusRule:\n  enabled: false\ndashboards:\n  enabled: false\n"), "PrometheusRule"); n != 0 {
		t.Error("PrometheusRule rendered with the switch off")
	}
}

// M3-A2/A3: the gateway reads pods, namespaces and Cilium endpoints and creates token reviews, and is
// granted nothing else; its binding names its own ServiceAccount.
func TestGatewayRBACIsReadOnlyPlusTokenReview(t *testing.T) {
	docs := render(t, "")
	role := find(t, docs, "ClusterRole", "afs-gateway")
	got := map[string]string{}
	rules, _ := role["rules"].([]any)
	for _, r := range rules {
		m, _ := r.(obj)
		groups, _ := m["apiGroups"].([]any)
		resources, _ := m["resources"].([]any)
		verbs, _ := m["verbs"].([]any)
		for _, res := range resources {
			vs := make([]string, 0, len(verbs))
			for _, v := range verbs {
				vs = append(vs, v.(string))
			}
			got[groups[0].(string)+"/"+res.(string)] = strings.Join(vs, ",")
		}
	}
	want := map[string]string{"/pods": "get,list,watch", "/namespaces": "get,list,watch",
		"cilium.io/ciliumendpoints": "get,list,watch", "authentication.k8s.io/tokenreviews": "create"}
	if len(got) != len(want) {
		t.Fatalf("rules = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s verbs = %q, want %q", k, got[k], v)
		}
	}
	b := find(t, docs, "ClusterRoleBinding", "afs-gateway")
	subs, _ := b["subjects"].([]any)
	s, _ := subs[0].(obj)
	if str(s, "name") != "afs-gateway" || str(s, "namespace") != "afs-system" {
		t.Errorf("subject = %v", s)
	}
	if env := envOf(podSpec(find(t, docs, "Deployment", "afs-gateway"))["containers"].([]any)[0].(obj)); env["AFS_GATEWAY_ROLE_ARN"] == "" {
		t.Error("the gateway has no AFS_GATEWAY_ROLE_ARN")
	}
}

// M4: the reconciler has a ClusterRole for the kinds it renders, bound to its own ServiceAccount, with a
// leader-lease Role; nothing grants it RBAC or node access, and the role-link and export settings are set.
func TestReconcilerRBAC(t *testing.T) {
	docs := render(t, "")
	role := find(t, docs, "ClusterRole", "afs-reconciler")
	rules, _ := role["rules"].([]any)
	if len(rules) == 0 {
		t.Fatal("no rules")
	}
	seen := map[string]bool{}
	for _, r := range rules {
		m, _ := r.(obj)
		groups, _ := m["apiGroups"].([]any)
		resources, _ := m["resources"].([]any)
		for _, g := range groups {
			if g.(string) == "rbac.authorization.k8s.io" {
				t.Errorf("the reconciler must not manage RBAC: %v", m)
			}
		}
		for _, res := range resources {
			seen[res.(string)] = true
		}
	}
	for _, want := range []string{"namespaces", "statefulsets", "networkpolicies", "ciliumnetworkpolicies", "nodepools", "volumesnapshots", "validatingadmissionpolicies"} {
		if !seen[want] {
			t.Errorf("the reconciler cannot reach %s", want)
		}
	}
	for _, bad := range []string{"nodes", "clusterroles", "clusterrolebindings", "pods/exec", "tokenreviews"} {
		if seen[bad] {
			t.Errorf("the reconciler must not reach %s", bad)
		}
	}
	for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
		b := find(t, docs, kind, map[string]string{"ClusterRoleBinding": "afs-reconciler", "RoleBinding": "afs-reconciler-leader"}[kind])
		subs, _ := b["subjects"].([]any)
		if s, _ := subs[0].(obj); str(s, "name") != "afs-reconciler" || str(s, "namespace") != "afs-system" {
			t.Errorf("%s subject = %v", kind, s)
		}
	}
	env := envOf(podSpec(find(t, docs, "Deployment", "afs-reconciler"))["containers"].([]any)[0].(obj))
	for _, k := range []string{"AFS_RECONCILER_ROLE_ARN", "AFS_EXPORT_BUCKET", "AFS_EXPORT_KMS_KEY_ID"} {
		if env[k] == "" {
			t.Errorf("the reconciler has no %s", k)
		}
	}
}

// M4: the billing CronJobs carry the report and showback jobs on their schedules and the reports bucket.
func TestBillingReportJobs(t *testing.T) {
	docs := render(t, "")
	want := map[string]string{"afs-billing-report-schedules": "*/15 * * * *", "afs-billing-showback": "0 6 * * *"}
	for name, schedule := range want {
		cj := find(t, docs, "CronJob", name)
		if got := str(path(cj, "spec"), "schedule"); got != schedule {
			t.Errorf("%s schedule = %q, want %q", name, got, schedule)
		}
	}
}

// RA-US-21: the backfill is a post-install and post-upgrade hook that runs `afs-api orchestration-backfill`
// with the API's service account and database login; it can be turned off.
func TestOrchestrationBackfillHook(t *testing.T) {
	docs := render(t, "")
	var job obj
	for _, d := range docs {
		if str(d, "kind") == "Job" && strings.Contains(str(path(d, "metadata"), "name"), "orchestration-backfill") {
			job = d
		}
	}
	if job == nil {
		t.Fatalf("no backfill job in %v", keys(docs))
	}
	if h := str(path(job, "metadata", "annotations"), "helm.sh/hook"); h != "post-install,post-upgrade" {
		t.Errorf("hook = %q", h)
	}
	ps := podSpec(job)
	c := ps["containers"].([]any)[0].(obj)
	if args := c["args"].([]any); len(args) != 1 || args[0] != "orchestration-backfill" || ps["serviceAccountName"] != "afs-api" || ps["restartPolicy"] != "Never" {
		t.Errorf("job: args %v sa %v", c["args"], ps["serviceAccountName"])
	}
	if env := envOf(c); !strings.Contains(env["AFS_DATABASE_URL"], "afs_app@") {
		t.Errorf("database url = %q", env["AFS_DATABASE_URL"])
	}
	for _, d := range render(t, "api:\n  orchestrationBackfill:\n    enabled: false\n") {
		if str(d, "kind") == "Job" {
			t.Errorf("disabled backfill still renders %s", key(d))
		}
	}
}

// The deprecation window is configurable and reaches afs-api as environment.
func TestDeprecationWindowEnv(t *testing.T) {
	docs := render(t, "api:\n  deprecation:\n    since: \"2026-11-23T00:00:00Z\"\n    sunset: \"2027-11-23T00:00:00Z\"\n")
	env := envOf(podSpec(find(t, docs, "Deployment", "afs-api"))["containers"].([]any)[0].(obj))
	if env["AFS_DEPRECATION_SINCE"] != "2026-11-23T00:00:00Z" || env["AFS_DEPRECATION_SUNSET"] != "2027-11-23T00:00:00Z" {
		t.Errorf("env: %v", env)
	}
}
