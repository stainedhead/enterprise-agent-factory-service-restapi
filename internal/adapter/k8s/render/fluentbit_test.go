package render_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/redact"
)

func logOutputs() []logs.Destination {
	return []logs.Destination{
		{ID: "log_01JD0000000000000000000005", Name: "dd", Kind: domain.LogDatadog, Enabled: true, SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/dd-AbCdEf",
			Config: map[string]string{"site": "datadoghq.com", "service": "payments"}},
		{ID: "log_01JD0000000000000000000001", Name: "cw", Kind: domain.LogCloudWatch, Enabled: true,
			Config: map[string]string{"logGroup": "/afs/payments", "region": "us-east-1"}},
		{ID: "log_01JD0000000000000000000002", Name: "s3", Kind: domain.LogS3, Enabled: true,
			Config: map[string]string{"bucket": "team-logs", "region": "us-east-1", "prefix": "afs/"}},
		{ID: "log_01JD0000000000000000000003", Name: "otel", Kind: domain.LogOTLP, Enabled: true, SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/otel-AbCdEf",
			Config: map[string]string{"endpoint": "https://otel.example.com:4318"}},
		{ID: "log_01JD0000000000000000000004", Name: "hec", Kind: domain.LogSplunkHEC, Enabled: true, SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/hec-AbCdEf",
			Config: map[string]string{"endpoint": "https://splunk.example.com:8088", "index": "afs"}},
	}
}

func logSpec() logs.Spec {
	return logs.Spec{Team: logs.Desired{TeamID: "team_01JD0000000000000000000001", Slug: "x", Namespace: "afs-t-x", State: "active"},
		Outputs: logOutputs(), RedactionKey: []byte("0123456789abcdef0123456789abcdef"), Image: logs.DefaultImage}
}

func TestFluentBitConfig(t *testing.T) {
	conf, err := render.FluentBitConfig(logOutputs())
	if err != nil {
		t.Fatal(err)
	}
	// The redaction filter comes before every output, and there is one output per destination, in id order.
	filter, firstOut := strings.Index(conf, "[FILTER]"), strings.Index(conf, "[OUTPUT]")
	if filter < 0 || firstOut < 0 || filter > firstOut || strings.Count(conf, "[OUTPUT]") != 5 || strings.Count(conf, "[FILTER]") != 1 {
		t.Fatalf("pipeline order wrong:\n%s", conf)
	}
	order := []string{"log_01JD0000000000000000000001", "log_01JD0000000000000000000002", "log_01JD0000000000000000000003", "log_01JD0000000000000000000004", "log_01JD0000000000000000000005"}
	last := 0
	for _, id := range order {
		i := strings.Index(conf, "# "+id)
		if i < last {
			t.Fatalf("%s out of order", id)
		}
		last = i
	}
	flat := regexp.MustCompile(` +`).ReplaceAllString(conf, " ")
	for _, want := range []string{"cloudwatch_logs", "log_group_name /afs/payments", "bucket team-logs", "opentelemetry",
		"splunk_token ${AFS_LOG_01JD0000000000000000000004}", "apikey ${AFS_LOG_01JD0000000000000000000005}",
		"http-intake.logs.datadoghq.com", "header Authorization Bearer ${AFS_LOG_01JD0000000000000000000003}"} {
		if !strings.Contains(flat, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	// No credential is ever rendered: only environment references.
	if strings.Contains(conf, "arn:aws:secretsmanager") {
		t.Fatal("a secret ARN is in the configuration")
	}
	// Removing a destination removes its output (RC-US-16.4).
	fewer, _ := render.FluentBitConfig(logOutputs()[1:])
	if strings.Contains(fewer, "datadog") || strings.Count(fewer, "[OUTPUT]") != 4 {
		t.Fatal("deleted destination still has an output")
	}
	if _, err := render.FluentBitConfig([]logs.Destination{{ID: "log_X", Kind: "syslog"}}); err == nil {
		t.Fatal("unknown kind rendered")
	}
}

func TestLogAggregatorObjects(t *testing.T) {
	agg, err := render.LogAggregatorFor(logSpec(), "")
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]*unstructured.Unstructured{}
	for _, o := range agg.Objects {
		if !render.IsManaged(o) {
			t.Errorf("%s/%s not managed", o.GetKind(), o.GetName())
		}
		if o.GetNamespace() != "afs-t-x" && !(o.GetKind() == "ConfigMap" && o.GetNamespace() == render.DefaultLogRouteNamespace) {
			t.Errorf("%s/%s in %s", o.GetKind(), o.GetName(), o.GetNamespace())
		}
		byKind[o.GetKind()+"/"+o.GetName()] = o
	}
	for _, k := range []string{"ServiceAccount/afs-log-aggregator", "ConfigMap/afs-log-aggregator", "Secret/afs-log-redaction-key", "ExternalSecret/afs-log-redaction-values",
		"ExternalSecret/afs-log-credentials", "Deployment/afs-log-aggregator", "Service/afs-log-aggregator", "CiliumNetworkPolicy/afs-log-aggregator", "ConfigMap/afs-log-route-x"} {
		if byKind[k] == nil {
			t.Errorf("missing %s", k)
		}
	}
	// Credentials: one ExternalSecret entry per destination with a secret, by ARN; the value is never read.
	data, _, _ := unstructured.NestedSlice(byKind["ExternalSecret/afs-log-credentials"].Object, "spec", "data")
	if len(data) != 3 {
		t.Fatalf("credential entries = %d, want 3", len(data))
	}
	// The bound secret values are found by the team tag.
	vals, _, _ := unstructured.NestedSlice(byKind["ExternalSecret/afs-log-redaction-values"].Object, "spec", "dataFrom")
	if got, _, _ := unstructured.NestedString(vals[0].(map[string]any), "find", "tags", "afs-team-id"); got != "team_01JD0000000000000000000001" {
		t.Fatalf("find tags: %v", vals)
	}
	// Only the aggregator mounts the key and the values.
	dep := byKind["Deployment/afs-log-aggregator"]
	vols, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes")
	mounts := 0
	for _, v := range vols {
		if strings.Contains(strings.ToLower(toJSON(t, v)), "redaction") {
			mounts++
		}
	}
	if mounts != 1 {
		t.Fatalf("redaction secrets mounted by %d volumes, want one projected volume", mounts)
	}
	// Restricted Pod Security: non-root, no escalation, read-only root, dropped capabilities.
	for _, want := range []string{`"runAsNonRoot":true`, `"allowPrivilegeEscalation":false`, `"readOnlyRootFilesystem":true`, `"drop":["ALL"]`, `"seccompProfile":{"type":"RuntimeDefault"}`} {
		if !strings.Contains(toJSON(t, dep.Object), want) {
			t.Errorf("deployment lacks %s", want)
		}
	}
	// A config change changes the pod template hash, so the pod rolls (Fluent Bit does not reload).
	h1, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", "afs.stainedhead.io/config-hash")
	spec2 := logSpec()
	spec2.Outputs = spec2.Outputs[1:]
	agg2, _ := render.LogAggregatorFor(spec2, "")
	var h2 string
	for _, o := range agg2.Objects {
		if o.GetKind() == "Deployment" {
			h2, _, _ = unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", "afs.stainedhead.io/config-hash")
		}
	}
	if h1 == "" || h1 == h2 {
		t.Fatalf("config hash did not change: %q %q", h1, h2)
	}
	// A destination set without secrets has no credentials ExternalSecret, and says it must be removed.
	noSecret := logSpec()
	noSecret.Outputs = noSecret.Outputs[1:3]
	agg3, _ := render.LogAggregatorFor(noSecret, "")
	for _, o := range agg3.Objects {
		if o.GetName() == render.LogCredentialsName {
			t.Fatal("credentials ExternalSecret rendered without a secret")
		}
	}
	if len(agg3.Optional) != 1 || agg3.Optional[0].Name != render.LogCredentialsName {
		t.Fatalf("optional = %+v", agg3.Optional)
	}
}

func TestLogAggregatorEgressIsOnlyTheDestinations(t *testing.T) {
	got := map[string]bool{}
	for _, e := range render.EgressTargets(logOutputs()) {
		got[e.Host+":"+e.Port] = true
	}
	for _, want := range []string{"logs.us-east-1.amazonaws.com:443", "s3.us-east-1.amazonaws.com:443", "team-logs.s3.us-east-1.amazonaws.com:443",
		"otel.example.com:4318", "splunk.example.com:8088", "http-intake.logs.datadoghq.com:443"} {
		if !got[want] {
			t.Errorf("missing egress %s", want)
		}
	}
	if len(got) != 6 {
		t.Errorf("egress targets %v", got)
	}
	agg, _ := render.LogAggregatorFor(logSpec(), "")
	for _, o := range agg.Objects {
		if o.GetKind() == "CiliumNetworkPolicy" {
			j := toJSON(t, o.Object)
			if !strings.Contains(j, render.PodIdentityAgent) || !strings.Contains(j, `"app.kubernetes.io/name":"fluent-bit"`) {
				t.Fatalf("policy %s", j)
			}
		}
	}
	// Without an AWS output the aggregator does not reach the Pod Identity agent.
	only := logSpec()
	only.Outputs = only.Outputs[3:4]
	agg, _ = render.LogAggregatorFor(only, "")
	for _, o := range agg.Objects {
		if o.GetKind() == "CiliumNetworkPolicy" && strings.Contains(toJSON(t, o.Object), render.PodIdentityAgent) {
			t.Fatal("pod identity agent reachable without an AWS output")
		}
	}
}

// The Lua filter is the twin of the Go redactor: both give the same text for every line. Skipped when
// luajit is not installed (CI installs it).
func TestRedactLuaMatchesGo(t *testing.T) {
	lua, err := exec.LookPath("luajit")
	if err != nil {
		t.Skip("luajit is not installed")
	}
	const secretA, secretB = "s3cr3t/value+with\"chars", "another-bound-secret"
	key := []byte("team-a-key-0123456789")
	r, err := redact.New(key, []byte(secretA), []byte(secretB), []byte("tiny"))
	if err != nil {
		t.Fatal(err)
	}
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r"
	corpus := []string{
		"login ok password=hunter2secret user=bob",
		`{"user":"bob","password":"p@ss w0rd!"}`,
		`DB_PASSWORD: 'correct horse'`,
		"GITHUB_TOKEN=ghp_abcdefghijklmnop1234 next=1",
		"api_key: sk-live-0123456789",
		"Authorization: Bearer abcdefgh12345678",
		"authorization:Basic dXNlcjpwYXNzd29yZA==",
		"curl -H 'Bearer zyxwvutsr98765'",
		"bearer authentication is required",
		"key AKIAIOSFODNN7EXAMPLE used and ASIAIOSFODNN7EXAMPLE",
		"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"token was " + jwt + " ok",
		"dial postgres://afs:s3cr3tpw@db.example.com:5432/afs and https://host/path",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY----- tail",
		"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkq",
		"request completed in 35ms",
		`{"max_tokens":100,"tokens_used":42,"secretId":"sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"}`,
		"password reset email sent", "token refresh scheduled", "password:", "password=", "id=AKIA short",
		"using " + secretA + " and again " + secretA + " and another-bound-secret end",
		`{"msg":"v=s3cr3t/value+with\"chars"}`, "?v=s3cr3t%2Fvalue%2Bwith%22chars", "tiny stays tiny",
		`"password": "" "token": ''`, `password="unterminated`, "secret=a:b token=c", "x-api-key: abc123",
		"passwd=[redacted:0123abcd] token=[redacted:0123abcd]", "client_secret=abc&token=def;pwd=ghi",
		"proxy-authorization: Bearer", "Bearer abc1", "bearer 12345678 and BASIC abcdefg9",
	}
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var in strings.Builder
	for _, c := range corpus {
		in.WriteString(base64.StdEncoding.EncodeToString([]byte(c)) + "\n")
	}
	vals := base64.StdEncoding.EncodeToString([]byte(secretA)) + "\n" + base64.StdEncoding.EncodeToString([]byte(secretB)) + "\n" + base64.StdEncoding.EncodeToString([]byte("tiny")) + "\n"
	script := write("redact.lua", []byte(render.RedactLua))
	driver := write("driver.lua", []byte(`
dofile(arg[1])
local R = AFS_REDACT
local key = io.open(arg[2], "rb"):read("*a")
local values = {}
for line in io.open(arg[3], "rb"):lines() do values[#values + 1] = R.b64decode(line) end
local lits = R.build_literals(key, values)
local B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
local function enc(s)
  return ((s:gsub(".", function(x) local r, b = "", x:byte() for i = 8, 1, -1 do r = r .. (b % 2 ^ i - b % 2 ^ (i - 1) > 0 and "1" or "0") end return r end) .. "0000"):gsub("%d%d%d?%d?%d?%d?", function(x)
    if #x < 6 then return "" end
    local c = 0 for i = 1, 6 do c = c + (x:sub(i, i) == "1" and 2 ^ (6 - i) or 0) end
    return B64:sub(c + 1, c + 1)
  end) .. ({ "", "==", "=" })[#s % 3 + 1])
end
for line in io.open(arg[4], "rb"):lines() do
  print(enc(R.redact_string(R.b64decode(line), key, lits)))
end
`))
	cmd := exec.Command(lua, driver, script, write("key", key), write("values", []byte(vals)), write("in", []byte(in.String())))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("luajit: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("lua printed %d lines for %d inputs:\n%s", len(lines), len(corpus), out)
	}
	changed := 0
	for i, c := range corpus {
		got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i]))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want := r.Redact(c)
		if want != c {
			changed++
		}
		if string(got) != want {
			t.Errorf("case %d %q\n lua: %q\n  go: %q", i, c, got, want)
		}
	}
	if changed < 20 {
		t.Fatalf("only %d corpus lines were redacted: the comparison proves little", changed)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// M2-H4: the reconciler may send sign-in records to the forward port, from its own namespace and
// component only; without a configured namespace only the node collector is admitted.
func TestAggregatorAdmitsReconcilerForSignIns(t *testing.T) {
	ingress := func(spec logs.Spec) []any {
		agg, err := render.LogAggregatorFor(spec, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range agg.Objects {
			if o.GetKind() == "CiliumNetworkPolicy" {
				rules, _, _ := unstructured.NestedSlice(o.Object, "spec", "ingress")
				return rules
			}
		}
		t.Fatal("no policy")
		return nil
	}
	if rules := ingress(logSpec()); len(rules) != 1 {
		t.Fatalf("without a reconciler namespace: %v", rules)
	}
	spec := logSpec()
	spec.ReconcilerNamespace = "afs-system"
	rules := ingress(spec)
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	from := rules[1].(map[string]any)["fromEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if from["k8s:io.kubernetes.pod.namespace"] != "afs-system" || from["app.kubernetes.io/component"] != "reconciler" || len(from) != 2 {
		t.Errorf("reconciler selector = %v", from)
	}
	ports := rules[1].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["ports"].([]any)
	if len(ports) != 1 || ports[0].(map[string]any)["port"] != "24224" {
		t.Errorf("ports = %v", ports)
	}
}
