package render

import (
	"crypto/sha256"
	_ "embed" // redact.lua
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

// Names of a team's log aggregator objects (RC-18). They live in the team namespace, except the route.
const (
	LogAggregatorName      = "afs-log-aggregator"
	LogRedactionKeyName    = "afs-log-redaction-key"
	LogRedactionValuesName = "afs-log-redaction-values"
	LogCredentialsName     = "afs-log-credentials"
	// LogForwardPort is the Fluent Bit forward port the node collector sends to.
	LogForwardPort = 24224
	// DefaultLogRouteNamespace holds the per-team route ConfigMaps the node collector assembles its
	// routing from (one output per team, matched on the namespace in the tag).
	DefaultLogRouteNamespace = "afs-logs"
	// LabelLogRoute marks a route ConfigMap.
	LabelLogRoute = "afs.stainedhead.io/log-route"
	// LogNodeCollectorLabel and Value select the node collector's pods, the only pods that may connect to
	// the aggregator.
	LogNodeCollectorLabel = "app.kubernetes.io/name"
	LogNodeCollectorValue = "fluent-bit"
)

// RedactLua is the redaction filter every aggregator runs before its outputs. It is the Lua twin of
// internal/usecase/redact; a parity test keeps them identical in behavior.
//
//go:embed redact.lua
var RedactLua string

// OwnerOfLogDelivery is the owner of every object of a team's log delivery.
func OwnerOfLogDelivery(d logs.Desired) Owner {
	return Owner{Kind: string(logs.Kind), ID: d.TeamID, TeamID: d.TeamID, TeamSlug: d.Slug}
}

// LogEnvName is the environment variable carrying a destination's credential into the aggregator
// ("AFS_LOG_" and the id's ULID). Configuration refers to it as ${NAME}; the value is never rendered.
func LogEnvName(destID string) string {
	_, ulid, _ := strings.Cut(destID, "_")
	return "AFS_LOG_" + strings.ToUpper(ulid)
}

// FluentBitConfig renders the aggregator's fluent-bit.conf: a forward input, the redaction filter on
// every record, and one output per destination in id order. A credential appears only as an
// environment reference.
func FluentBitConfig(outputs []logs.Destination) (string, error) {
	var b strings.Builder
	b.WriteString(`[SERVICE]
    Flush        1
    Daemon       Off
    Log_Level    info
    HTTP_Server  On
    HTTP_Listen  0.0.0.0
    HTTP_Port    2020
    Health_Check On
    storage.path /tmp/fluent-bit/storage

[INPUT]
    Name   forward
    Listen 0.0.0.0
    Port   ` + strconv.Itoa(LogForwardPort) + `

# Redaction runs before every output: nothing is delivered unredacted.
[FILTER]
    Name   lua
    Match  *
    script /fluent-bit/etc/redact.lua
    call   redact
`)
	outs := append([]logs.Destination(nil), outputs...)
	sort.Slice(outs, func(i, j int) bool { return outs[i].ID < outs[j].ID })
	for _, d := range outs {
		o, err := fluentBitOutput(d)
		if err != nil {
			return "", fmt.Errorf("destination %s: %w", d.ID, err)
		}
		b.WriteString("\n# " + d.ID + "\n" + o)
	}
	return b.String(), nil
}

func fluentBitOutput(d logs.Destination) (string, error) {
	c := d.Config
	env := "${" + LogEnvName(d.ID) + "}"
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "    %-22s %s\n", k, v) }
	b.WriteString("[OUTPUT]\n")
	switch d.Kind {
	case domain.LogCloudWatch:
		line("Name", "cloudwatch_logs")
		line("Match", "*")
		line("region", c["region"])
		line("log_group_name", c["logGroup"])
		prefix := c["logStreamPrefix"]
		if prefix == "" {
			prefix = "afs-"
		}
		line("log_stream_prefix", prefix)
		line("auto_create_group", "On")
	case domain.LogS3:
		line("Name", "s3")
		line("Match", "*")
		line("region", c["region"])
		line("bucket", c["bucket"])
		line("s3_key_format", "/"+c["prefix"]+"$TAG/%Y/%m/%d/%H/$UUID.gz")
		line("compression", "gzip")
		line("total_file_size", "50M")
		line("upload_timeout", "1m")
		line("store_dir", "/tmp/fluent-bit/s3")
	case domain.LogOTLP:
		u, err := url.Parse(c["endpoint"])
		if err != nil {
			return "", err
		}
		line("Name", "opentelemetry")
		line("Match", "*")
		line("host", u.Hostname())
		line("port", portOf(u))
		line("logs_uri", "/v1/logs")
		line("tls", "On")
		line("tls.verify", "On")
		if d.SecretARN != "" {
			line("header", "Authorization Bearer "+env)
		}
	case domain.LogSplunkHEC:
		u, err := url.Parse(c["endpoint"])
		if err != nil {
			return "", err
		}
		line("Name", "splunk")
		line("Match", "*")
		line("host", u.Hostname())
		line("port", portOf(u))
		line("tls", "On")
		line("tls.verify", "On")
		line("splunk_token", env)
		for _, kv := range [][2]string{{"event_index", c["index"]}, {"event_source", c["source"]}, {"event_sourcetype", c["sourcetype"]}} {
			if kv[1] != "" {
				line(kv[0], kv[1])
			}
		}
	case domain.LogDatadog:
		line("Name", "datadog")
		line("Match", "*")
		line("host", DatadogHost(c["site"]))
		line("tls", "On")
		line("apikey", env)
		if v := c["service"]; v != "" {
			line("dd_service", v)
		}
		if v := c["source"]; v != "" {
			line("dd_source", v)
		}
	default:
		return "", fmt.Errorf("unknown kind %q", d.Kind)
	}
	return b.String(), nil
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

// DatadogHost is the log intake host of a Datadog site.
func DatadogHost(site string) string { return "http-intake.logs." + site }

// EgressTarget is one allowed destination of the aggregator: a DNS name and a TCP port.
type EgressTarget struct {
	Host string
	Port string
}

// EgressTargets lists where the aggregator may connect for the outputs: the destinations' hosts on
// their ports, and nothing else.
func EgressTargets(outputs []logs.Destination) []EgressTarget {
	seen := map[EgressTarget]bool{}
	var out []EgressTarget
	add := func(h, p string) {
		t := EgressTarget{h, p}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, d := range outputs {
		c := d.Config
		switch d.Kind {
		case domain.LogCloudWatch:
			add("logs."+c["region"]+".amazonaws.com", "443")
		case domain.LogS3:
			add("s3."+c["region"]+".amazonaws.com", "443")
			add(c["bucket"]+".s3."+c["region"]+".amazonaws.com", "443")
		case domain.LogOTLP, domain.LogSplunkHEC:
			if u, err := url.Parse(c["endpoint"]); err == nil {
				add(u.Hostname(), portOf(u))
			}
		case domain.LogDatadog:
			add(DatadogHost(c["site"]), "443")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func usesAWS(outputs []logs.Destination) bool {
	for _, d := range outputs {
		if d.Kind == domain.LogCloudWatch || d.Kind == domain.LogS3 {
			return true
		}
	}
	return false
}

// PodIdentityAgent is the link-local address of the EKS Pod Identity agent, which the aggregator
// reaches for the AWS credentials of its CloudWatch and S3 outputs.
const PodIdentityAgent = "169.254.170.23/32"

// LogAggregator is the rendered aggregator of one team.
type LogAggregator struct {
	// Objects are applied in order.
	Objects []*unstructured.Unstructured
	// Optional names objects that exist only for some specs and must be deleted when absent: the
	// credentials ExternalSecret when no output uses a secret.
	Optional []OptionalObject
}

// OptionalObject identifies an object an apply may need to remove.
type OptionalObject struct {
	APIVersion, Kind, Namespace, Name string
}

// LogAggregatorFor renders the team's aggregator and its route. routeNamespace holds the route
// ConfigMap; empty uses DefaultLogRouteNamespace.
func LogAggregatorFor(spec logs.Spec, routeNamespace string) (LogAggregator, error) {
	if routeNamespace == "" {
		routeNamespace = DefaultLogRouteNamespace
	}
	ns, owner := spec.Team.Namespace, OwnerOfLogDelivery(spec.Team)
	conf, err := FluentBitConfig(spec.Outputs)
	if err != nil {
		return LogAggregator{}, err
	}
	var credEnvs []string
	for _, d := range spec.Outputs {
		if d.SecretARN != "" {
			credEnvs = append(credEnvs, LogEnvName(d.ID))
		}
	}
	sort.Strings(credEnvs)
	sum := sha256.Sum256([]byte(conf + "\x00" + RedactLua + "\x00" + strings.Join(credEnvs, ",")))
	hash := hex.EncodeToString(sum[:])
	selector := map[string]any{"app.kubernetes.io/name": LogAggregatorName}

	var objs []*unstructured.Unstructured
	add := func(u *unstructured.Unstructured) { objs = append(objs, Managed(u, owner)) }

	sa := New("v1", "ServiceAccount", ns, LogAggregatorName)
	sa.Object["automountServiceAccountToken"] = false
	add(sa)

	cm := New("v1", "ConfigMap", ns, LogAggregatorName)
	cm.Object["data"] = map[string]any{"fluent-bit.conf": conf, "redact.lua": RedactLua}
	add(cm)

	sec := New("v1", "Secret", ns, LogRedactionKeyName)
	sec.Object["type"] = "Opaque"
	sec.Object["data"] = map[string]any{"key": base64.StdEncoding.EncodeToString(spec.RedactionKey)}
	add(sec)

	// The bound secret values reach the filter through External Secrets, found by the team tag
	// (M2-C1 tags every secret afs-team-id), and are written into one file: a base64 value per line. The
	// reconciler never reads them.
	vals := New("external-secrets.io/v1", "ExternalSecret", ns, LogRedactionValuesName)
	vals.Object["spec"] = map[string]any{
		"refreshInterval": "1m",
		"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
		"target": map[string]any{
			"name": LogRedactionValuesName, "creationPolicy": "Owner",
			"template": map[string]any{"engineVersion": "v2", "data": map[string]any{
				"values": `{{ range $k, $v := . }}{{ $v | b64enc }}` + "\n" + `{{ end }}`,
			}},
		},
		"dataFrom": []any{map[string]any{"find": map[string]any{
			"tags": map[string]any{"afs-team-id": spec.Team.TeamID},
		}}},
	}
	add(vals)

	var optional []OptionalObject
	if len(credEnvs) > 0 {
		var data []any
		for _, d := range spec.Outputs {
			if d.SecretARN != "" {
				data = append(data, map[string]any{"secretKey": LogEnvName(d.ID), "remoteRef": map[string]any{"key": d.SecretARN}})
			}
		}
		sort.Slice(data, func(i, j int) bool {
			return data[i].(map[string]any)["secretKey"].(string) < data[j].(map[string]any)["secretKey"].(string)
		})
		creds := New("external-secrets.io/v1", "ExternalSecret", ns, LogCredentialsName)
		creds.Object["spec"] = map[string]any{
			"refreshInterval": "1m",
			"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
			"target":          map[string]any{"name": LogCredentialsName, "creationPolicy": "Owner"},
			"data":            data,
		}
		add(creds)
	} else {
		optional = append(optional, OptionalObject{"external-secrets.io/v1", "ExternalSecret", ns, LogCredentialsName})
	}

	add(aggregatorDeployment(spec, ns, selector, hash, credEnvs))

	svc := New("v1", "Service", ns, LogAggregatorName)
	svc.Object["spec"] = map[string]any{
		"selector": selector,
		"ports":    []any{map[string]any{"name": "forward", "port": int64(LogForwardPort), "targetPort": "forward", "protocol": "TCP"}},
	}
	add(svc)

	add(aggregatorPolicy(spec, ns))

	route := New("v1", "ConfigMap", routeNamespace, "afs-log-route-"+spec.Team.Slug)
	route.Object["data"] = map[string]any{"route.conf": routeConfig(spec.Team)}
	l := map[string]string{LabelLogRoute: "true"}
	route.SetLabels(l)
	add(route)

	return LogAggregator{Objects: objs, Optional: optional}, nil
}

// routeConfig is the node collector's output for one team: records whose tag carries the team's
// namespace are forwarded to its aggregator.
func routeConfig(d logs.Desired) string {
	return fmt.Sprintf(`[OUTPUT]
    Name        forward
    Match_Regex ^kube\..*_%s_.*
    Host        %s.%s.svc.cluster.local
    Port        %d
`, strings.ReplaceAll(d.Namespace, "-", `\-`), LogAggregatorName, d.Namespace, LogForwardPort)
}

func aggregatorDeployment(spec logs.Spec, ns string, selector map[string]any, hash string, credEnvs []string) *unstructured.Unstructured {
	var env []any
	for _, e := range credEnvs {
		env = append(env, map[string]any{"name": e, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": LogCredentialsName, "key": e}}})
	}
	container := map[string]any{
		"name": "fluent-bit", "image": spec.Image, "imagePullPolicy": "IfNotPresent",
		"ports": []any{
			map[string]any{"name": "forward", "containerPort": int64(LogForwardPort), "protocol": "TCP"},
			map[string]any{"name": "http", "containerPort": int64(2020), "protocol": "TCP"},
		},
		"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/api/v1/health", "port": "http"}, "periodSeconds": int64(10)},
		"livenessProbe":  map[string]any{"httpGet": map[string]any{"path": "/", "port": "http"}, "periodSeconds": int64(30)},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
			"limits":   map[string]any{"memory": "256Mi"},
		},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}},
		},
		"volumeMounts": []any{
			map[string]any{"name": "config", "mountPath": "/fluent-bit/etc", "readOnly": true},
			map[string]any{"name": "redact", "mountPath": "/etc/afs/redact", "readOnly": true},
			map[string]any{"name": "tmp", "mountPath": "/tmp"},
		},
	}
	if len(env) > 0 {
		container["env"] = env
	}
	u := New("apps/v1", "Deployment", ns, LogAggregatorName)
	u.Object["spec"] = map[string]any{
		"replicas": int64(1),
		"selector": map[string]any{"matchLabels": selector},
		"strategy": map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": int64(0), "maxSurge": int64(1)}},
		"template": map[string]any{
			"metadata": map[string]any{
				"labels":      selector,
				"annotations": map[string]any{"afs.stainedhead.io/config-hash": hash},
			},
			"spec": map[string]any{
				"serviceAccountName":           LogAggregatorName,
				"automountServiceAccountToken": false,
				"securityContext": map[string]any{
					"runAsNonRoot": true, "runAsUser": int64(10001), "runAsGroup": int64(10001), "fsGroup": int64(10001),
					"seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
				"containers": []any{container},
				"volumes": []any{
					map[string]any{"name": "config", "configMap": map[string]any{"name": LogAggregatorName}},
					// Only this pod mounts the redaction key and the bound secret values.
					map[string]any{"name": "redact", "projected": map[string]any{"sources": []any{
						map[string]any{"secret": map[string]any{"name": LogRedactionKeyName, "items": []any{map[string]any{"key": "key", "path": "key"}}}},
						map[string]any{"secret": map[string]any{"name": LogRedactionValuesName, "items": []any{map[string]any{"key": "values", "path": "values"}}}},
					}}},
					map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
				},
			},
		},
	}
	return u
}

// aggregatorPolicy admits the node collector to the forward port and lets the aggregator reach only
// its destinations (the default deny of the namespace still covers everything else).
func aggregatorPolicy(spec logs.Spec, ns string) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", ns, LogAggregatorName)
	var egress []any
	for _, t := range EgressTargets(spec.Outputs) {
		egress = append(egress, map[string]any{
			"toFQDNs": []any{map[string]any{"matchName": t.Host}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": t.Port, "protocol": "TCP"}}}},
		})
	}
	if usesAWS(spec.Outputs) {
		egress = append(egress, map[string]any{
			"toCIDR":  []any{PodIdentityAgent},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "80", "protocol": "TCP"}}}},
		})
	}
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": LogAggregatorName}},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{
				podNamespaceLabel: DefaultLogRouteNamespace, LogNodeCollectorLabel: LogNodeCollectorValue,
			}}},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": strconv.Itoa(LogForwardPort), "protocol": "TCP"}}}},
		}},
		"egress": egress,
	}
	return u
}
