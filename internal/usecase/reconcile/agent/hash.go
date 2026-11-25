package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sort"
)

// PodTemplateHash is the hash of everything the pod is built from that a person or a rotation can change:
// the image, persona, model, resources, the resolved configuration values, the delivered secret
// bindings with the version of each secret, the AWS profiles and the tags. It is the pod-template
// annotation that rolls the pod when one of the referenced versions changes (RC-US-2.2). The replica
// count and the restart nonce are not part of it: pausing does not roll the pod, and a restart has its own
// annotation. Secret values are never read, only the version id of each secret.
func PodTemplateHash(d Desired) string {
	h := sha256.New()
	w := func(format string, args ...any) { _, _ = io.WriteString(h, fmt.Sprintf(format, args...)+"\n") }
	w("image=%s", d.Image)
	w("persona=%s", d.Persona)
	w("model=%s models=%v", d.ModelID, d.Models)
	w("cpu=%s memory=%s", d.Resources.CPU, d.Resources.Memory)
	w("identity=%s", d.IAMIdentity)
	w("tier=%s workorder=%t", d.Team.IsolationTier, d.Team.WorkOrderAssigned)
	if l := RoutesHash(d.Team, d.Routes); l != "" {
		w("%s", l)
	}
	if l := McpHash(d.Team, d.Mcp); l != "" {
		w("%s", l)
	}
	if d.Collab != nil {
		w("collab tool=%s mode=%s host=%s prefix=%s", d.Collab.Tool, d.Collab.Mode, d.Collab.Host, d.Collab.SecretsPrefix)
	}
	if d.Link != nil {
		w("collab link tool=%s id=%s", d.Link.Tool, d.Link.PaperclipAgentID)
	}

	cfg := slices.Clone(d.Config)
	sort.Slice(cfg, func(i, j int) bool { return cfg[i].Name < cfg[j].Name })
	for _, c := range cfg {
		w("config %s=%s", c.Name, c.Value)
	}
	binds := slices.Clone(d.Bindings)
	sort.Slice(binds, func(i, j int) bool { return binds[i].Delivery+binds[i].Target < binds[j].Delivery+binds[j].Target })
	for _, b := range binds {
		if !b.Delivered() {
			continue
		}
		w("binding %s %s -> %s version=%s", b.Delivery, b.Target, b.SecretID, b.VersionID)
	}
	links := slices.Clone(d.RoleLinks)
	sort.Slice(links, func(i, j int) bool { return links[i].ProfileName < links[j].ProfileName })
	for _, l := range links {
		if l.Status == "disabled" {
			continue
		}
		w("link %s %s %s %d", l.ProfileName, l.RoleARN, l.ExternalID, l.MaxSessionSeconds)
	}
	keys := make([]string, 0, len(d.Tags))
	for k := range d.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("tag %s=%s", k, d.Tags[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
