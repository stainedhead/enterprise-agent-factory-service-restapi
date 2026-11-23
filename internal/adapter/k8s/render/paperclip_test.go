package render_test

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

func collabDesired() agent.Desired {
	d := agentDesired()
	d.Role = agent.RoleCollaboration
	d.Collab = &agent.Collab{Mode: "manual", Host: "alpha.collab.example.com"}
	return d
}

// RC-21: the Service `paperclip` fronts the collaboration agent on port 3100.
func TestPaperclipService(t *testing.T) {
	d := collabDesired()
	u := render.CollabService(d)
	if u.GetName() != "paperclip" || u.GetNamespace() != "afs-t-alpha" || u.GetKind() != "Service" {
		t.Fatalf("%s/%s %s", u.GetNamespace(), u.GetName(), u.GetKind())
	}
	sel, _, _ := nestedMap(u.Object, "spec", "selector")
	if sel[render.LabelAgentID] != aID {
		t.Errorf("selector %v", sel)
	}
	ports, _, _ := nestedSlice(u.Object, "spec", "ports")
	if len(ports) != 1 || ports[0].(map[string]any)["port"] != int64(3100) {
		t.Errorf("ports %v", ports)
	}
	if !render.IsManaged(u) || render.OwnerOf(u).ID != aID {
		t.Error("not owned by the agent")
	}
}

// RC-23: ingress admits the team's pods and afs-edge only, on 3100 only; egress admits only the collaboration agent.
func TestPaperclipPolicies(t *testing.T) {
	d := collabDesired()
	in := render.PaperclipIngress(d, agent.EdgeRef{Namespace: "afs-system", Name: "afs-edge", ReconcilerName: "afs-reconciler"})
	rules, _, _ := nestedSlice(in.Object, "spec", "ingress")
	if len(rules) != 1 {
		t.Fatalf("rules %v", rules)
	}
	from := rules[0].(map[string]any)["fromEndpoints"].([]any)
	if len(from) != 3 {
		t.Errorf("sources %v", from)
	}
	toPorts := rules[0].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["ports"].([]any)
	if len(toPorts) != 1 || toPorts[0].(map[string]any)["port"] != "3100" {
		t.Errorf("ports %v", toPorts)
	}
	if noEdge := render.PaperclipIngress(d, agent.EdgeRef{}); len(noEdge.Object["spec"].(map[string]any)["ingress"].([]any)[0].(map[string]any)["fromEndpoints"].([]any)) != 1 {
		t.Error("an unset edge reference must not widen the policy")
	}
	eg := render.PaperclipEgress(d)
	out, _, _ := nestedSlice(eg.Object, "spec", "egress")
	if len(out) != 1 || eg.GetName() != render.PaperclipEgressPolicyName {
		t.Errorf("egress %v", out)
	}
}

// RC-21: the Paperclip environment is on the pod only for the collaboration agent.
func TestPaperclipEnv(t *testing.T) {
	env := func(d agent.Desired) map[string]string {
		m := map[string]string{}
		for _, c := range render.PodTemplate(environment(d)).Spec.Containers {
			for _, e := range c.Env {
				m[e.Name] = e.Value
			}
		}
		return m
	}
	got := env(collabDesired())
	if got["PORT"] != "3100" || got["PAPERCLIP_PUBLIC_URL"] != "https://alpha.collab.example.com" || got["PAPERCLIP_DEPLOYMENT_MODE"] != "authenticated" ||
		got["PAPERCLIP_DEPLOYMENT_EXPOSURE"] != "public" || got["AFS_PAPERCLIP_BOOTSTRAP"] != "manual" ||
		got["PAPERCLIP_ALLOWED_HOSTNAMES"] != "alpha.collab.example.com,paperclip.afs-t-alpha.svc.cluster.local" {
		t.Errorf("env %v", got)
	}
	d := collabDesired()
	d.Collab = &agent.Collab{Mode: "managed", SecretsPrefix: "afs/afs-t-alpha/paperclip/paperclip/"} //nolint:gosec // a path prefix, not a credential
	got = env(d)
	if got["AFS_PAPERCLIP_BOOTSTRAP"] != "managed" || got["PAPERCLIP_PUBLIC_URL"] != "" || got["PAPERCLIP_SECRETS_AWS_PREFIX"] == "" ||
		got["PAPERCLIP_ALLOWED_HOSTNAMES"] != "paperclip.afs-t-alpha.svc.cluster.local" {
		t.Errorf("managed env %v", got)
	}
	// A connected worker learns where Paperclip is, carries the label the ingress policy admits and gets its id.
	w := agentDesired()
	w.Link = &agent.CollabLink{PaperclipAgentID: "pc-1"}
	got = env(w)
	if got["PAPERCLIP_API_URL"] != "http://paperclip.afs-t-alpha.svc.cluster.local:3100" || got["PAPERCLIP_AGENT_ID"] != "pc-1" {
		t.Errorf("worker env %v", got)
	}
	if render.PodTemplate(environment(w)).Labels[render.LabelCollabConnected] != "true" {
		t.Error("connected worker has no connected label")
	}
	w.Link = &agent.CollabLink{}
	if _, ok := env(w)["PAPERCLIP_AGENT_ID"]; ok {
		t.Error("an unregistered worker got an agent id")
	}
	if render.PodTemplate(environment(agentDesired())).Labels[render.LabelCollabConnected] != "" {
		t.Error("an unconnected worker carries the connected label")
	}
	if _, ok := env(agentDesired())["PAPERCLIP_PUBLIC_URL"]; ok {
		t.Error("a worker got the Paperclip environment")
	}
	// A change of mode or host rolls the pod.
	if agent.PodTemplateHash(collabDesired()) == agent.PodTemplateHash(d) || agent.PodTemplateHash(agentDesired()) == agent.PodTemplateHash(w) {
		t.Error("collab settings are not in the pod-template hash")
	}
}

// RC-23: team namespaces refuse Ingress and Gateway API routes, externalIPs and the paperclip Service of anyone but the reconciler.
func TestPaperclipAdmission(t *testing.T) {
	objs := render.AdmissionObjects(admission.Spec{ReconcilerUsers: []string{"afs-reconciler"}})
	exprs := map[string]string{}
	for _, o := range objs {
		if o.GetKind() != "ValidatingAdmissionPolicy" {
			continue
		}
		vals, _, _ := nestedSlice(o.Object, "spec", "validations")
		for _, v := range vals {
			exprs[o.GetName()] += v.(map[string]any)["expression"].(string) + "\n"
		}
	}
	for name, want := range map[string]string{
		"afs-deny-external-routes":      "false",
		"afs-deny-external-services":    "externalIPs",
		"afs-protect-paperclip-service": "'paperclip'",
	} {
		if !strings.Contains(exprs[name], want) {
			t.Errorf("%s: %q not in %q", name, want, exprs[name])
		}
	}
	if !strings.Contains(exprs["afs-deny-external-services"], "loadBalancerSourceRanges") ||
		!strings.Contains(exprs["afs-protect-paperclip-service"], `"afs-reconciler"`) {
		t.Error("service rules incomplete")
	}
	routes := ""
	for _, o := range objs {
		if o.GetName() == "afs-deny-external-routes" && o.GetKind() == "ValidatingAdmissionPolicy" {
			rules, _, _ := nestedSlice(o.Object, "spec", "matchConstraints", "resourceRules")
			for _, r := range rules {
				routes += strings.Join(toStrings(r.(map[string]any)["apiGroups"].([]any)), ",") + ";"
			}
		}
	}
	if routes != "networking.k8s.io;gateway.networking.k8s.io;" {
		t.Errorf("route groups %q", routes)
	}
}

func toStrings(in []any) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.(string)
	}
	return out
}

// ORC-8, ORC-9, ORC-12: with Buzz the Service is `buzz` on port 3000, the policies use 3000, the orchestration
// agent gets the relay environment and a connected worker the tool-neutral variables plus BUZZ_RELAY_URL.
func TestBuzzRendering(t *testing.T) {
	d := collabDesired()
	d.Collab = &agent.Collab{Tool: agent.ToolBuzz, Mode: "managed"}
	svc := render.CollabService(d)
	ports, _, _ := nestedSlice(svc.Object, "spec", "ports")
	if svc.GetName() != "buzz" || ports[0].(map[string]any)["port"] != int64(3000) {
		t.Fatalf("service %s %v", svc.GetName(), ports)
	}
	in := render.PaperclipIngress(d, agent.EdgeRef{})
	toPorts := in.Object["spec"].(map[string]any)["ingress"].([]any)[0].(map[string]any)["toPorts"].([]any)[0].(map[string]any)["ports"].([]any)
	if toPorts[0].(map[string]any)["port"] != "3000" {
		t.Errorf("ingress ports %v", toPorts)
	}
	env := render.PaperclipEnv(d, *d.Collab)
	if env["PORT"] != "3000" || env["AFS_COLLAB_TOOL"] != "buzz" || env["BUZZ_RELAY_HOSTNAME"] != "buzz.afs-t-alpha.svc.cluster.local" || env["PAPERCLIP_PUBLIC_URL"] != "" {
		t.Errorf("env %v", env)
	}
	w := render.CollabWorkerEnv(d, agent.CollabLink{Tool: agent.ToolBuzz})
	if w["BUZZ_RELAY_URL"] != "ws://buzz.afs-t-alpha.svc.cluster.local:3000" || w["AFS_COLLAB_URL"] != w["BUZZ_RELAY_URL"] || w["PAPERCLIP_API_URL"] != "" {
		t.Errorf("worker env %v", w)
	}
	pw := render.CollabWorkerEnv(d, agent.CollabLink{PaperclipAgentID: "pc-1"})
	if pw["AFS_COLLAB_TOOL"] != "paperclip" || pw["PAPERCLIP_API_URL"] != "http://paperclip.afs-t-alpha.svc.cluster.local:3100" || pw["PAPERCLIP_AGENT_ID"] != "pc-1" {
		t.Errorf("paperclip worker env %v", pw)
	}
}

// MOD-6: the pod gets the default model and the effective models as environment and as /etc/afs/models.json.
func TestModelsDelivery(t *testing.T) {
	if got := render.ModelsJSON("b", []string{"b", "a"}); got != `{"default":"b","models":["a","b"]}` {
		t.Errorf("json %s", got)
	}
	if render.ModelsEnv([]string{"b", "a"}) != "a,b" || render.ModelsEnv(nil) != "" {
		t.Error("env")
	}
	d := agentDesired()
	d.ModelID, d.Models = "m2", []string{"m2", "m1"}
	cm := render.AgentAWSConfigMap(d)
	data, _, _ := nestedMap(cm.Object, "data")
	if data["models.json"] != `{"default":"m2","models":["m1","m2"]}` {
		t.Errorf("configmap %v", data)
	}
}
