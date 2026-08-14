{{- define "dashboard.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "dashboard.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name (include "dashboard.name" .) | trunc 63 | trimSuffix "-" }}{{ end -}}
{{- end -}}
{{- define "dashboard.labels" -}}
app.kubernetes.io/name: {{ include "dashboard.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- define "dashboard.selector" -}}
app.kubernetes.io/name: {{ include "dashboard.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "dashboard.validate" -}}
{{- if not (has .Values.environment (list "dev" "stage" "prod")) }}{{ fail "environment must be dev, stage, or prod" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (or (eq .Values.api.config.AUTH_MODE "mock") (eq .Values.api.config.AUTH_MODE "none")) }}{{ fail "stage/prod require AUTH_MODE=oidc" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (ne (toString .Values.api.config.USE_DEMO_DATA) "false") }}{{ fail "stage/prod require USE_DEMO_DATA=false" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (or (not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.ui.image.digest)) (not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.api.image.digest))) }}{{ fail "stage/prod UI and API images require valid sha256 digests" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (or (lt (int .Values.ui.replicas) 2) (lt (int .Values.api.replicas) 2) (not .Values.ui.pdb.enabled) (not .Values.api.pdb.enabled) (not .Values.ui.autoscaling.enabled) (not .Values.api.autoscaling.enabled) (lt (int .Values.ui.autoscaling.minReplicas) 2) (lt (int .Values.api.autoscaling.minReplicas) 2)) }}{{ fail "stage/prod require two replicas, HPA, and PDB for UI/API" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (not .Values.secretRevision) }}{{ fail "stage/prod require secretRevision for explicit secret rollout tracking" }}{{ end -}}
{{- if and (ne .Values.environment "dev") .Values.redis.enabled (or (not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.redis.image.digest)) (not .Values.redis.persistence.enabled)) }}{{ fail "stage/prod bundled Redis requires immutable digest and persistence" }}{{ end -}}
{{- if and (not .Values.redis.enabled) (not .Values.api.existingSecret.name) }}{{ fail "disabled bundled Redis requires existingSecret.name containing REDIS_ADDR" }}{{ end -}}
{{- if and (eq .Values.environment "prod") (not .Values.networkPolicy.external) }}{{ fail "prod requires explicit external network destinations" }}{{ end -}}
{{- if and (not .Values.networkPolicy.ingress.cidrs) (or (empty .Values.networkPolicy.ingress.namespaceSelector) (empty .Values.networkPolicy.ingress.podSelector)) }}{{ fail "UI ingress requires at least one CIDR or nonempty namespace+pod selectors" }}{{ end -}}
{{- if and .Values.networkPolicy.monitoring.enabled (or (empty .Values.networkPolicy.monitoring.namespaceSelector) (empty .Values.networkPolicy.monitoring.podSelector)) }}{{ fail "monitoring ingress requires nonempty namespace+pod selectors" }}{{ end -}}
{{- if or (empty .Values.networkPolicy.dns.namespaceSelector) (empty .Values.networkPolicy.dns.podSelector) }}{{ fail "DNS egress requires nonempty namespace+pod selectors" }}{{ end -}}
{{- $purposes := dict -}}
{{- range .Values.networkPolicy.external -}}
{{- if hasKey $purposes .purpose }}{{ fail (printf "duplicate external network purpose: %s" .purpose) }}{{ end -}}
{{- $_ := set $purposes .purpose true -}}
{{- end -}}
{{- end -}}
