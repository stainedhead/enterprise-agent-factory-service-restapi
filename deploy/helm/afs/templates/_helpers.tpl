{{/* Component values: values/<name>.yaml merged under .Values.<name>. Usage:
     {{ $c := include "afs.component" (dict "root" $ "name" "api") | fromYaml }} */}}
{{- define "afs.component" -}}
{{- $base := (.root.Files.Get (printf "values/%s.yaml" .name) | fromYaml) | default dict -}}
{{- $over := (index .root.Values .name) | default dict -}}
{{- toYaml (mergeOverwrite $base (deepCopy $over)) -}}
{{- end -}}

{{- define "afs.name" -}}afs-{{ . }}{{- end -}}

{{- define "afs.labels" -}}
app.kubernetes.io/name: {{ include "afs.name" .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .name }}
app.kubernetes.io/part-of: afs
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version }}
{{- end -}}

{{- define "afs.selectorLabels" -}}
app.kubernetes.io/name: {{ include "afs.name" .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{- define "afs.image" -}}
{{- $g := .root.Values.global.image -}}
{{- $reg := required "global.image.registry is required" $g.registry -}}
{{- printf "%s/%s:%s" $reg .c.image.repository (default .root.Chart.AppVersion $g.tag) -}}
{{- end -}}

{{/* Database URL without a password: the login is the component's role, IAM auth supplies the token. */}}
{{- define "afs.databaseURL" -}}
{{- $d := .root.Values.database -}}
{{- printf "postgres://%s@%s:%v/%s?sslmode=%s" .user (required "database.host is required" $d.host) $d.port $d.name $d.sslmode -}}
{{- end -}}

{{/* Environment shared by every binary, then the component's own env map (sorted). */}}
{{- define "afs.env" -}}
- name: AFS_DATABASE_URL
  value: {{ include "afs.databaseURL" (dict "root" .root "user" .c.dbUser) | quote }}
- name: AFS_DB_IAM_AUTH
  value: {{ .root.Values.database.iamAuth | quote }}
- name: AFS_AWS_REGION
  value: {{ required "global.region is required" .root.Values.global.region | quote }}
- name: AFS_CLUSTER_NAME
  value: {{ required "global.clusterName is required" .root.Values.global.clusterName | quote }}
- name: AFS_LOG_LEVEL
  value: {{ .root.Values.global.logLevel | quote }}
{{- with .root.Values.global.otlpEndpoint }}
- name: AFS_OTLP_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- range $k, $v := (.c.env | default dict) }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{- define "afs.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "afs.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{/* Replicas spread across availability zones, and across nodes where possible. */}}
{{- define "afs.spread" -}}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: {{ .root.Values.global.topologyKey }}
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        {{- include "afs.selectorLabels" . | nindent 8 }}
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "afs.selectorLabels" . | nindent 8 }}
{{- end -}}

{{/* Probes against the operations port (/healthz, /readyz). */}}
{{- define "afs.probes" -}}
livenessProbe:
  httpGet: {path: /healthz, port: metrics}
  periodSeconds: 10
  failureThreshold: 3
readinessProbe:
  httpGet: {path: /readyz, port: metrics}
  periodSeconds: 5
  failureThreshold: 2
startupProbe:
  httpGet: {path: /healthz, port: metrics}
  periodSeconds: 2
  failureThreshold: 30
{{- end -}}
