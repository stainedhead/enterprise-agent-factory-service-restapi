package tests

import (
	"strings"
	"testing"
)

// DP-2: afs-edge is the one public component. It runs as the afs_edge role, takes its sealing keys from a
// Secret, accepts the ALB and Prometheus only, and reaches the collaboration pods on 3100.
func TestEdgeDeployment(t *testing.T) {
	docs := render(t, "")
	d := find(t, docs, "Deployment", "afs-edge")
	ps := podSpec(d)
	c := ps["containers"].([]any)[0].(obj)
	env := map[string]obj{}
	for _, e := range c["env"].([]any) {
		m := e.(obj)
		env[str(m, "name")] = m
	}
	for _, name := range []string{"AFS_EDGE_STATE_KEY", "AFS_EDGE_STATE_KEY_PREVIOUS"} {
		ref := path(env[name], "valueFrom", "secretKeyRef")
		if str(ref, "name") != "afs-edge-state-key" || str(ref, "key") == "" {
			t.Errorf("%s must come from the state key Secret: %v", name, env[name])
		}
		if _, hasValue := env[name]["value"]; hasValue {
			t.Errorf("%s has a literal value", name)
		}
	}
	if path(env["AFS_EDGE_STATE_KEY_PREVIOUS"], "valueFrom", "secretKeyRef")["optional"] != true {
		t.Error("the previous key is optional")
	}
	if env["AFS_EDGE_ADDR"]["value"] != ":8443" || env["AFS_EDGE_TLS_CERT"]["value"] != "/tls/tls.crt" || env["AFS_EDGE_IDLE_TIMEOUT"]["value"] != "10m" {
		t.Errorf("edge env: %v", env)
	}
	if n, _ := ps["terminationGracePeriodSeconds"].(int); n < 40 {
		t.Errorf("terminationGracePeriodSeconds = %v: the drain needs time", ps["terminationGracePeriodSeconds"])
	}
	svc := find(t, docs, "Service", "afs-edge")
	ports := path(svc, "spec")["ports"].([]any)
	if len(ports) != 2 || path(ports[0].(obj))["port"] != 443 || path(ports[0].(obj))["targetPort"] != "https" {
		t.Errorf("service ports: %v", ports)
	}
	find(t, docs, "ServiceAccount", "afs-edge")
	cert := find(t, docs, "Certificate", "afs-edge")
	if path(cert, "spec")["secretName"] != "afs-edge-tls" || path(cert, "spec", "issuerRef")["name"] != "afs-ca" {
		t.Errorf("certificate: %v", path(cert, "spec"))
	}
	if vols, _ := ps["volumes"].([]any); len(vols) != 1 || path(vols[0].(obj), "secret")["secretName"] != "afs-edge-tls" {
		t.Errorf("the edge does not mount its certificate: %v", ps["volumes"])
	}
	for _, a := range []string{"AFSEdgeErrorRatioHigh", "AFSEdgeRouteRefreshFailing", "AFSEdgeAccessCountsDropped"} {
		if !strings.Contains(renderedAlerts(t, docs), a) {
			t.Errorf("alert %s missing", a)
		}
	}
}

func renderedAlerts(t *testing.T, docs []obj) string {
	t.Helper()
	var sb strings.Builder
	rule := find(t, docs, "PrometheusRule", "afs-slis")
	for _, g := range path(rule, "spec")["groups"].([]any) {
		for _, r := range g.(obj)["rules"].([]any) {
			sb.WriteString(str(r.(obj), "alert") + "\n")
		}
	}
	return sb.String()
}

func TestEdgeNetworkPolicy(t *testing.T) {
	docs := render(t, "edge:\n  network:\n    albCidrs: [\"10.1.0.0/20\"]\n    databaseCidrs: [\"10.2.0.0/24\"]\n")
	np := find(t, docs, "NetworkPolicy", "afs-edge")
	spec := path(np, "spec")
	ing := spec["ingress"].([]any)
	if len(ing) != 3 {
		t.Fatalf("ingress rules (https, metrics, ALB health check): %v", ing)
	}
	if hc := ing[2].(obj)["from"].([]any); path(ing[2].(obj)["ports"].([]any)[0].(obj))["port"] != 9090 || path(hc[0].(obj), "ipBlock")["cidr"] != "10.1.0.0/20" {
		t.Errorf("health check rule: %v", ing[2])
	}
	http := ing[0].(obj)
	if path(http["ports"].([]any)[0].(obj))["port"] != 8443 {
		t.Errorf("http rule: %v", http)
	}
	from := http["from"].([]any)
	if len(from) != 1 || path(from[0].(obj), "ipBlock")["cidr"] != "10.1.0.0/20" {
		t.Errorf("only the ALB subnets may reach the edge: %v", from)
	}
	metrics := ing[1].(obj)
	if path(metrics["from"].([]any)[0].(obj), "namespaceSelector", "matchLabels")["kubernetes.io/metadata.name"] != "monitoring" {
		t.Errorf("metrics rule: %v", metrics)
	}
	var to3100 bool
	for _, r := range spec["egress"].([]any) {
		rule := r.(obj)
		p, _ := rule["ports"].([]any)
		if len(p) == 1 && path(p[0].(obj))["port"] == 3100 {
			to := rule["to"].([]any)
			to3100 = path(to[0].(obj), "namespaceSelector", "matchLabels")["afs.stainedhead.io/team-namespace"] == "true"
		}
	}
	if !to3100 {
		t.Error("the edge must reach team namespaces on 3100")
	}
	// the default allows any source for the ALB port (the chart cannot know the subnets)
	def := path(find(t, render(t, ""), "NetworkPolicy", "afs-edge"), "spec")["ingress"].([]any)[0].(obj)
	if _, restricted := def["from"]; restricted {
		t.Error("with no albCidrs the https rule has no from")
	}
}

func TestEdgeTargetGroupBindingAndLogRouting(t *testing.T) {
	for _, d := range render(t, "") {
		if str(d, "kind") == "TargetGroupBinding" {
			t.Fatal("no target group, no binding")
		}
	}
	arn := "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/afs-edge/0123456789abcdef"
	docs := render(t, "edge:\n  targetGroupArn: "+arn+"\n")
	b := find(t, docs, "TargetGroupBinding", "afs-edge")
	spec := path(b, "spec")
	if spec["targetGroupARN"] != arn || spec["targetType"] != "ip" || path(spec, "serviceRef")["name"] != "afs-edge" {
		t.Errorf("binding: %v", spec)
	}
	cm := find(t, docs, "ConfigMap", "afs-edge-log-routing")
	conf := str(path(cm, "data"), "edge-routing.conf")
	if !strings.Contains(conf, "afs.team_id") || !strings.Contains(conf, "afs-system_afs-edge") {
		t.Errorf("log routing: %s", conf)
	}
}

func TestEdgeCanBeDisabled(t *testing.T) {
	for _, d := range render(t, "edge:\n  enabled: false\n") {
		if strings.Contains(key(d), "afs-edge") {
			t.Errorf("disabled edge still renders %s", key(d))
		}
	}
}

// DP-2: the only public path is the ALB through the TargetGroupBinding; the chart renders no Ingress, no
// LoadBalancer and no NodePort for the edge.
func TestEdgeIsNotExposedByTheChart(t *testing.T) {
	docs := render(t, "edge:\n  targetGroupArn: arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/afs-edge/0123456789abcdef\n")
	for _, d := range docs {
		if str(d, "kind") == "Ingress" {
			t.Error("the chart renders an Ingress")
		}
		if str(d, "kind") == "Service" {
			if typ := str(path(d, "spec"), "type"); typ == "LoadBalancer" || typ == "NodePort" {
				t.Errorf("%s is a %s Service", key(d), typ)
			}
		}
	}
}
