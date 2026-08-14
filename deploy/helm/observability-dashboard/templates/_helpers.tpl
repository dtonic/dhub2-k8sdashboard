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
{{- if and .Values.dashboardBuilder.enabled (or (empty .Values.api.existingSecret.name) (empty .Values.dashboardBuilder.databaseURLKey) (empty .Values.dashboardBuilder.cursorKeyKey) (not .Values.dashboardBuilder.postgresEgress.cidrs)) }}{{ fail "dashboard builder requires existingSecret keys and bounded PostgreSQL egress CIDRs" }}{{ end -}}
{{- if and .Values.dashboardBuilder.enabled (not (regexMatch "^(?:[1-9]|[12][0-9]|30)s$" .Values.dashboardBuilder.connectTimeout)) }}{{ fail "dashboard builder connectTimeout must be between 1s and 30s" }}{{ end -}}
{{- if and .Values.dashboardBuilder.enabled (ne .Values.environment "dev") (not .Values.dashboardBuilder.requireTLS) }}{{ fail "stage/prod dashboard builder requires verified PostgreSQL TLS" }}{{ end -}}
{{- if and (eq .Values.environment "prod") (not .Values.networkPolicy.external) }}{{ fail "prod requires explicit external network destinations" }}{{ end -}}
{{- if and (not .Values.networkPolicy.ingress.cidrs) (or (empty .Values.networkPolicy.ingress.namespaceSelector) (empty .Values.networkPolicy.ingress.podSelector)) }}{{ fail "UI ingress requires at least one CIDR or nonempty namespace+pod selectors" }}{{ end -}}
{{- if and .Values.networkPolicy.monitoring.enabled (or (empty .Values.networkPolicy.monitoring.namespaceSelector) (empty .Values.networkPolicy.monitoring.podSelector)) }}{{ fail "monitoring ingress requires nonempty namespace+pod selectors" }}{{ end -}}
{{- if or (empty .Values.networkPolicy.dns.namespaceSelector) (empty .Values.networkPolicy.dns.podSelector) }}{{ fail "DNS egress requires nonempty namespace+pod selectors" }}{{ end -}}
{{- $purposes := dict -}}
{{- range .Values.networkPolicy.external -}}
{{- if hasKey $purposes .purpose }}{{ fail (printf "duplicate external network purpose: %s" .purpose) }}{{ end -}}
{{- $_ := set $purposes .purpose true -}}
{{- end -}}
{{- $mode := .Values.telemetry.mode -}}
{{- if and (ne $mode "disabled") (empty .Values.telemetry.clusterName) }}{{ fail "enabled telemetry requires clusterName" }}{{ end -}}
{{- if ne $mode "disabled" -}}
{{- if not .Values.telemetry.collectorBuildVerified }}{{ fail "enabled telemetry requires verified custom collector build acknowledgment" }}{{ end -}}
{{- $registry := first (splitList "/" .Values.telemetry.image.repository) -}}
{{- if or (not (contains "/" .Values.telemetry.image.repository)) (and (not (contains "." $registry)) (not (contains ":" $registry))) (hasSuffix ".invalid" $registry) (eq .Values.telemetry.image.repository "otel/opentelemetry-collector-contrib") }}{{ fail "enabled telemetry requires an operator-published custom collector repository with explicit registry host" }}{{ end -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.telemetry.image.digest) }}{{ fail "telemetry collector image requires a valid sha256 digest" }}{{ end -}}
{{- end -}}
{{- if ge (int .Values.telemetry.gateway.pdb.minAvailable) (int .Values.telemetry.gateway.replicas) }}{{ fail "telemetry gateway PDB minAvailable must be less than replicas" }}{{ end -}}
{{- $limiter := int .Values.telemetry.memoryLimiter.limitMiB -}}{{- $spike := int .Values.telemetry.memoryLimiter.spikeLimitMiB -}}
{{- if ge $spike $limiter }}{{ fail "telemetry memory limiter spikeLimitMiB must be less than limitMiB" }}{{ end -}}
{{- range $component := list .Values.telemetry.agent .Values.telemetry.gateway .Values.telemetry.clusterCollector -}}
{{- $podMemory := toString $component.resources.limits.memory -}}
{{- if not (regexMatch "^[1-9][0-9]*Mi$" $podMemory) }}{{ fail "telemetry collector memory limits must use positive Mi units" }}{{ end -}}
{{- if ge $limiter (int (trimSuffix "Mi" $podMemory)) }}{{ fail "telemetry memory limiter limitMiB must be below every collector pod memory limit" }}{{ end -}}
{{- end -}}
{{- if eq $mode "cutover" -}}
{{- if not .Values.telemetry.existingLogCollectionDisabled }}{{ fail "telemetry cutover requires existingLogCollectionDisabled=true" }}{{ end -}}
{{- if not .Values.telemetry.existingMetricCollectionDisabled }}{{ fail "telemetry cutover requires existingMetricCollectionDisabled=true" }}{{ end -}}
{{- if or (empty .Values.telemetry.existingSecret.name) (empty .Values.telemetry.backends.greptime.endpoint) (empty .Values.telemetry.backends.quickwit.endpoint) }}{{ fail "telemetry cutover requires existingSecret and both backend endpoints" }}{{ end -}}
{{- if ne .Values.telemetry.backends.quickwit.index "otel-logs-v0_7" }}{{ fail "telemetry cutover requires the verified Quickwit otel-logs-v0_7 schema" }}{{ end -}}
{{- if or (empty .Values.api.config.GREPTIME_URL) (empty .Values.api.config.QUICKWIT_URL) (ne (toString .Values.api.config.USE_DEMO_DATA) "false") }}{{ fail "telemetry cutover requires BFF Greptime/Quickwit query URLs and USE_DEMO_DATA=false" }}{{ end -}}
{{- if or (not .Values.telemetry.comparison.recorded) (empty .Values.telemetry.comparison.evidenceId) (empty .Values.telemetry.comparison.artifactHash) (empty .Values.telemetry.comparison.startedAt) (empty .Values.telemetry.comparison.endedAt) (lt (int .Values.telemetry.comparison.windowMinutes) 30) (lt (int64 .Values.telemetry.comparison.baselineEvents) 1) (lt (int64 .Values.telemetry.comparison.candidateEvents) 1) }}{{ fail "telemetry cutover requires complete comparison evidence over at least 30 minutes" }}{{ end -}}
{{- if and (ne .Values.environment "dev") (or (ne .Values.telemetry.comparison.kind "production-comparison") (ne .Values.telemetry.comparison.evidenceId (printf "%s/%s" .Values.environment .Values.telemetry.comparison.artifactHash))) }}{{ fail "stage/prod telemetry requires production evidence linked by environment/artifactHash" }}{{ end -}}
{{- if and (eq .Values.environment "dev") (or (ne .Values.telemetry.comparison.kind "local-deterministic-validation") (ne .Values.telemetry.comparison.evidenceId (printf "local-fixture/%s" .Values.telemetry.comparison.artifactHash))) }}{{ fail "dev cutover fixture requires local evidence linked by artifactHash" }}{{ end -}}
{{- $c := .Values.telemetry.comparison -}}{{- $l := $c.limits -}}
{{- $derivedLoss := 0 -}}{{- if lt (int64 $c.candidateEvents) (int64 $c.baselineEvents) -}}{{- $derivedLoss = div (mul (sub (int64 $c.baselineEvents) (int64 $c.candidateEvents)) 1000) (int64 $c.baselineEvents) -}}{{- end -}}
{{- if ne (int64 $c.lossPermille) (int64 $derivedLoss) }}{{ fail "telemetry comparison lossPermille does not match baseline/candidate counts" }}{{ end -}}
{{- if or (gt (int $c.lossPermille) (int $l.maxLossPermille)) (gt (int $c.p95LatencyMs) (int $l.maxP95LatencyMs)) (gt (int $c.collectorCpuMillicores) (int $l.maxCollectorCpuMillicores)) (gt (int $c.collectorMemoryMiB) (int $l.maxCollectorMemoryMiB)) (gt (int64 $c.egressBytesPerHour) (int64 $l.maxEgressBytesPerHour)) (gt (int64 $c.storageBytesPerDay) (int64 $l.maxStorageBytesPerDay)) (gt (int64 $c.estimatedCostMicrosPerDay) (int64 $l.maxEstimatedCostMicrosPerDay)) }}{{ fail "telemetry cutover comparison exceeds an operator-defined limit" }}{{ end -}}
{{- $telemetryPurposes := dict -}}
{{- range .Values.telemetry.backends.egress -}}{{- $_ := set $telemetryPurposes .purpose true -}}{{- end -}}
{{- if or (ne (len .Values.telemetry.backends.egress) 2) (not (hasKey $telemetryPurposes "greptime")) (not (hasKey $telemetryPurposes "quickwit")) }}{{ fail "telemetry cutover requires one bounded egress destination for each backend" }}{{ end -}}
{{- $quickwitPort := trimPrefix ":" (regexFind ":[0-9]+$" .Values.telemetry.backends.quickwit.endpoint) -}}
{{- $greptimePort := trimPrefix ":" (trimSuffix "/v1/otlp" (regexFind ":[0-9]+/v1/otlp$" .Values.telemetry.backends.greptime.endpoint)) -}}
{{- range .Values.telemetry.backends.egress -}}
{{- if and (eq .purpose "quickwit") (ne (int $quickwitPort) (int .port)) }}{{ fail "Quickwit endpoint port must match its NetworkPolicy egress port" }}{{ end -}}
{{- if and (eq .purpose "greptime") (ne (int $greptimePort) (int .port)) }}{{ fail "Greptime endpoint port must match its NetworkPolicy egress port" }}{{ end -}}
{{- end -}}
{{- end -}}
{{- end -}}
