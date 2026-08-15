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
{{- define "dashboard.validIPv4CIDR" -}}
{{- $parts := splitList "/" . -}}
{{- if ne (len $parts) 2 }}{{ fail "central Registry agent source CIDR is invalid" }}{{ end -}}
{{- $octets := splitList "." (index $parts 0) -}}
{{- if or (ne (len $octets) 4) (not (regexMatch "^[0-9]+$" (index $parts 1))) (lt (atoi (index $parts 1)) 1) (gt (atoi (index $parts 1)) 32) }}{{ fail "central Registry agent source CIDR is invalid" }}{{ end -}}
{{- range $octets }}{{- if or (not (regexMatch "^[0-9]{1,3}$" .)) (gt (atoi .) 255) }}{{ fail "central Registry agent source CIDR is invalid" }}{{ end -}}{{- end -}}
{{- if eq . "0.0.0.0/0" }}{{ fail "central Registry agent source CIDR is invalid" }}{{ end -}}
{{- end -}}
{{- define "dashboard.validate" -}}
{{- $central := eq .Values.clusterState.mode "central" -}}
{{- if not (has .Values.clusterState.mode (list "direct" "central")) }}{{ fail "clusterState.mode must be direct or central" }}{{ end -}}
{{- if $central -}}
{{- if ne .Values.api.config.AUTH_MODE "oidc" }}{{ fail "central mode requires AUTH_MODE=oidc" }}{{ end -}}
{{- if ne (toString .Values.api.config.USE_DEMO_DATA) "false" }}{{ fail "central mode requires USE_DEMO_DATA=false" }}{{ end -}}
{{- if or (empty .Values.clusterState.clusters) (gt (len .Values.clusterState.clusters) (int .Values.clusterState.limits.maxClusters)) }}{{ fail "central mode requires bounded configured clusters" }}{{ end -}}
{{- $seenClusters := dict -}}{{- range .Values.clusterState.clusters -}}{{- if or (not (regexMatch "^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$" .)) (hasKey $seenClusters .) }}{{ fail "central cluster IDs must be unique canonical IDs" }}{{ end -}}{{- $_ := set $seenClusters . true -}}{{- end -}}
{{- if or (empty .Values.clusterState.trustDomain) (empty .Values.clusterState.registryEndpoint) (empty .Values.clusterState.registryServerName) (empty .Values.clusterState.tls.apiExistingSecret) (empty .Values.clusterState.tls.registryExistingSecret) (eq .Values.clusterState.tls.apiExistingSecret .Values.clusterState.tls.registryExistingSecret) }}{{ fail "central mode requires distinct API and Registry mTLS Secrets plus endpoint identity" }}{{ end -}}
{{- if not (regexMatch "^[A-Za-z0-9.-]+:[0-9]{1,5}$" .Values.clusterState.registryEndpoint) }}{{ fail "central registry endpoint must be host:port" }}{{ end -}}
{{- if ne .Values.clusterState.registryEndpoint (printf "%s-cluster-state-registry:%d" (include "dashboard.fullname" .) (int .Values.clusterState.registry.queryPort)) }}{{ fail "central API endpoint must target the chart Registry query Service" }}{{ end -}}
{{- $dnsName := "^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$" -}}
{{- if or (gt (len .Values.clusterState.registryServerName) 253) (not (regexMatch $dnsName .Values.clusterState.registryServerName)) (not (regexMatch "^[a-z0-9.-]+$" .Values.clusterState.trustDomain)) }}{{ fail "central TLS server name or trust domain is invalid" }}{{ end -}}
{{- if or (empty .Values.clusterState.tls.certKey) (empty .Values.clusterState.tls.privateKeyKey) (empty .Values.clusterState.tls.caKey) (eq .Values.clusterState.tls.certKey .Values.clusterState.tls.privateKeyKey) (eq .Values.clusterState.tls.certKey .Values.clusterState.tls.caKey) (eq .Values.clusterState.tls.privateKeyKey .Values.clusterState.tls.caKey) }}{{ fail "central TLS Secret keys must be distinct and nonempty" }}{{ end -}}
{{- if or (ne (int .Values.clusterState.registry.replicas) 1) (not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.clusterState.registry.image.digest)) }}{{ fail "central registry must be singleton with an immutable image digest" }}{{ end -}}
{{- if not (has .Values.clusterState.registry.agentService.type (list "ClusterIP" "LoadBalancer")) }}{{ fail "central Registry agent Service type is invalid" }}{{ end -}}
{{- if empty .Values.clusterState.registry.agentService.sourceCidrs }}{{ fail "central Registry agent ingress requires bounded source CIDRs" }}{{ end -}}
{{- range .Values.clusterState.registry.agentService.sourceCidrs }}{{ include "dashboard.validIPv4CIDR" . }}{{- end -}}
{{- if eq (int .Values.clusterState.registry.agentPort) (int .Values.clusterState.registry.queryPort) }}{{ fail "central agent and query ports must be distinct" }}{{ end -}}
{{- if or (lt (int .Values.clusterState.limits.maxClusters) 1) (gt (int .Values.clusterState.limits.maxClusters) 64) (lt (int .Values.clusterState.limits.maxResources) 1) (gt (int .Values.clusterState.limits.maxResources) 100000) (lt (int .Values.clusterState.limits.maxChunkResources) 1) (gt (int .Values.clusterState.limits.maxChunkResources) 1000) (lt (int .Values.clusterState.limits.maxMessageBytes) 1024) (gt (int .Values.clusterState.limits.maxMessageBytes) 4194304) }}{{ fail "central registry limits are outside safe bounds" }}{{ end -}}
{{- if gt (int .Values.clusterState.limits.maxChunkResources) (int .Values.clusterState.limits.maxResources) }}{{ fail "central registry chunk count exceeds resource capacity" }}{{ end -}}
{{- if or (gt (int64 .Values.clusterState.limits.maxMessageBytes) (int64 .Values.clusterState.limits.maxStateBytes)) (gt (int64 .Values.clusterState.limits.maxStateBytes) (int64 .Values.clusterState.limits.maxTotalStateBytes)) (gt (int64 .Values.clusterState.limits.maxTotalStateBytes) 536870912) }}{{ fail "central registry retained byte caps are invalid" }}{{ end -}}
{{- $registryMemory := toString .Values.clusterState.registry.resources.limits.memory -}}
{{- if not (regexMatch "^[1-9][0-9]*Mi$" $registryMemory) }}{{ fail "central Registry memory limit must use Mi units" }}{{ end -}}
{{- if lt (mul (int64 (trimSuffix "Mi" $registryMemory)) 1048576) (mul (int64 .Values.clusterState.limits.maxTotalStateBytes) 2) }}{{ fail "central Registry memory limit requires 2x retained-byte headroom" }}{{ end -}}
{{- $frameRateText := toString .Values.clusterState.limits.ingressFrameRate -}}{{- $byteRateText := toString .Values.clusterState.limits.ingressByteRate -}}
{{- if or (not (regexMatch "^[0-9]+(?:\\.[0-9]+)?(?:e[+-]?[0-9]+)?$" $frameRateText)) (not (regexMatch "^[0-9]+(?:\\.[0-9]+)?(?:e[+-]?[0-9]+)?$" $byteRateText)) }}{{ fail "central registry ingress rates must be finite numeric values" }}{{ end -}}
{{- if or (lt (float64 .Values.clusterState.limits.ingressFrameRate) 1.0) (gt (float64 .Values.clusterState.limits.ingressFrameRate) 100000.0) (lt (float64 .Values.clusterState.limits.ingressByteRate) 1024.0) (gt (float64 .Values.clusterState.limits.ingressByteRate) 1073741824.0) (lt (int .Values.clusterState.limits.ingressFrameBurst) 1) (gt (int .Values.clusterState.limits.ingressFrameBurst) 100000) (lt (int .Values.clusterState.limits.ingressByteBurst) 1024) (gt (int64 .Values.clusterState.limits.ingressByteBurst) 1073741824) }}{{ fail "central registry ingress limits are invalid" }}{{ end -}}
{{- if lt (int64 .Values.clusterState.limits.ingressByteBurst) (int64 .Values.clusterState.limits.maxMessageBytes) }}{{ fail "central registry byte burst must admit one maximum message" }}{{ end -}}
{{- $staleText := toString .Values.clusterState.limits.staleTTL -}}{{- $heartbeatText := toString .Values.clusterState.limits.heartbeatTimeout -}}
{{- if or (not (regexMatch "^[1-9][0-9]*(?:s|m|h)$" $staleText)) (not (regexMatch "^[1-9][0-9]*(?:s|m|h)$" $heartbeatText)) }}{{ fail "central registry durations must be positive s, m, or h values" }}{{ end -}}
{{- $staleMultiplier := 1 -}}{{- if hasSuffix "m" $staleText }}{{- $staleMultiplier = 60 -}}{{- else if hasSuffix "h" $staleText }}{{- $staleMultiplier = 3600 -}}{{- end -}}
{{- $heartbeatMultiplier := 1 -}}{{- if hasSuffix "m" $heartbeatText }}{{- $heartbeatMultiplier = 60 -}}{{- else if hasSuffix "h" $heartbeatText }}{{- $heartbeatMultiplier = 3600 -}}{{- end -}}
{{- $staleSeconds := mul (int64 (trimSuffix "h" (trimSuffix "m" (trimSuffix "s" $staleText)))) $staleMultiplier -}}
{{- $heartbeatSeconds := mul (int64 (trimSuffix "h" (trimSuffix "m" (trimSuffix "s" $heartbeatText)))) $heartbeatMultiplier -}}
{{- if or (gt $staleSeconds 86400) (gt $heartbeatSeconds 3600) (gt $heartbeatSeconds $staleSeconds) }}{{ fail "central registry durations are outside safe bounds" }}{{ end -}}
{{- end -}}

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
