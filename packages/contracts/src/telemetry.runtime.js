/**
 * 정본 텔레메트리 계약(schema/telemetry.schema.json)의 런타임 구현입니다.
 *
 * 의도적으로 **플레인 ES 모듈(JS)**입니다 — Node `node --test`가 트랜스파일 없이
 * 직접 실행해 스키마와의 동등성·검증·상관키 동작을 증명합니다(코드젠·외부 의존성 없음).
 * 타입은 telemetry.runtime.d.ts가, 스키마와의 상수 동등성은 test/*.mjs가 보장합니다.
 */

/** Query Catalog(defaults/*.yaml)가 실제로 쓰는 모든 단위. 스키마 MetricUnit enum과 1:1. */
export const METRIC_UNITS = Object.freeze([
  "percent",
  "bytes",
  "bytes_per_sec",
  "count",
  "cores",
  "millicores",
  "mebibytes",
]);

/** 라벨 상한. 검증은 라벨 수 n에 대해 O(n)으로 유계입니다. */
export const TELEMETRY_LABEL_LIMITS = Object.freeze({
  maxCount: 32,
  maxKeyLength: 64,
  maxValueLength: 256,
});

/**
 * 신원·서비스·트레이스·메시지 필드 — 임의 라벨로 쓸 수 없습니다.
 * 정규화된 DTO 이름과 함께, EntityRef로 흡수되는 **원본 속성 별칭**
 * (cluster.id, k8s.*, service.*, trace_id/span_id)도 거부합니다.
 */
export const RESERVED_LABEL_KEYS = Object.freeze([
  "clusterId",
  "namespace",
  "workloadKind",
  "workloadName",
  "workloadUid",
  "podName",
  "podUid",
  "containerName",
  "serviceName",
  "serviceNamespace",
  "serviceVersion",
  "traceId",
  "spanId",
  "message",
  "cluster.id",
  "k8s.namespace.name",
  "k8s.workload.kind",
  "k8s.workload.name",
  "k8s.workload.uid",
  "k8s.pod.name",
  "k8s.pod.uid",
  "k8s.container.name",
  "service.name",
  "service.namespace",
  "service.version",
  "trace_id",
  "span_id",
]);

export const WORKLOAD_KINDS = Object.freeze([
  "Deployment",
  "StatefulSet",
  "DaemonSet",
  "ReplicaSet",
  "CronJob",
]);

export const TELEMETRY_RECORD_TYPES = Object.freeze(["metric", "log", "event", "alert"]);

const LOG_LEVELS = ["ERROR", "WARN", "INFO", "DEBUG"];
const EVENT_TYPES = ["Normal", "Warning"];
const ALERT_SEVERITIES = ["critical", "warning", "info"];
const ALERT_STATUSES = ["firing", "resolved", "pending"];
const RESERVED_SET = new Set(RESERVED_LABEL_KEYS);
const UNIT_SET = new Set(METRIC_UNITS);

/**
 * CorrelationKey는 UID 우선순위(README §5)로 엔티티를 하나의 키로 접습니다.
 * Pod UID → Workload UID → ns+kind+name → Pod Name. 클러스터 신원을 항상 포함합니다.
 * 같은 Pod의 metric/log/event는 같은 키가 되고, 이름이 같아도 UID가 다르면
 * (재생성된 인스턴스) 다른 키가 됩니다. Go의 contract.CorrelationKey와 형식이 같습니다.
 */
export function correlationKey(entity) {
  if (!entity || typeof entity.clusterId !== "string" || entity.clusterId === "") {
    return "";
  }
  if (entity.podUid) {
    return `${entity.clusterId}|pod-uid|${entity.podUid}`;
  }
  if (entity.workloadUid) {
    return `${entity.clusterId}|workload-uid|${entity.workloadUid}`;
  }
  if (entity.namespace && entity.workloadKind && entity.workloadName) {
    return `${entity.clusterId}|workload|${entity.namespace}|${entity.workloadKind}|${entity.workloadName}`;
  }
  if (entity.podName) {
    return `${entity.clusterId}|pod-name|${entity.namespace ?? ""}|${entity.podName}`;
  }
  return entity.clusterId;
}

function err(errors, path, message) {
  errors.push({ path, message });
}

function optionalString(errors, path, value) {
  if (value === undefined) return;
  if (typeof value !== "string" || value === "") {
    err(errors, path, "비어 있지 않은 문자열이어야 합니다");
  }
}

/** validateEntityRef는 EntityRef의 정합성 규칙을 필드 경로와 함께 검사합니다. */
export function validateEntityRef(entity, path = "entity") {
  const errors = [];
  if (entity === null || typeof entity !== "object" || Array.isArray(entity)) {
    err(errors, path, "객체여야 합니다");
    return errors;
  }
  if (typeof entity.clusterId !== "string" || entity.clusterId === "") {
    err(errors, `${path}.clusterId`, "clusterId는 필수입니다");
  }
  for (const f of [
    "namespace",
    "workloadName",
    "workloadUid",
    "podName",
    "podUid",
    "containerName",
    "serviceName",
    "serviceNamespace",
    "serviceVersion",
  ]) {
    optionalString(errors, `${path}.${f}`, entity[f]);
  }
  if (entity.workloadKind !== undefined && !WORKLOAD_KINDS.includes(entity.workloadKind)) {
    err(errors, `${path}.workloadKind`, `허용되지 않는 workloadKind입니다: ${entity.workloadKind}`);
  }
  // README §5의 fallback 신원은 Namespace + Kind + Name 삼중쌍입니다.
  // kind와 name은 함께 있어야 합니다 (UID-only 워크로드는 kind/name 없이 유효).
  if (entity.workloadKind !== undefined && entity.workloadName === undefined) {
    err(errors, `${path}.workloadName`, "workloadKind가 있으면 workloadName도 있어야 합니다");
  }
  if (entity.workloadName !== undefined && entity.workloadKind === undefined) {
    err(errors, `${path}.workloadKind`, "workloadName이 있으면 workloadKind도 있어야 합니다");
  }
  // Pod 수준 신원은 이름·UID가 함께 있어야 정합합니다. 이름만 있으면 재생성된
  // 인스턴스와 섞이고, UID만 있으면 화면에 보여줄 이름이 없습니다.
  if (entity.podName !== undefined && entity.podUid === undefined) {
    err(errors, `${path}.podUid`, "podName이 있으면 podUid도 있어야 합니다");
  }
  if (entity.podUid !== undefined && entity.podName === undefined) {
    err(errors, `${path}.podName`, "podUid가 있으면 podName도 있어야 합니다");
  }
  if (entity.containerName !== undefined && (entity.podName === undefined || entity.podUid === undefined)) {
    err(errors, `${path}.containerName`, "containerName은 Pod 신원(podName+podUid)이 있어야 합니다");
  }
  // README §5 계층: Cluster → Namespace → Workload → Pod → Container.
  // Workload/Pod/Container 신원이 하나라도 있으면 namespace가 필요합니다 (오류는 1건만).
  if (
    entity.namespace === undefined &&
    (entity.workloadKind !== undefined ||
      entity.workloadName !== undefined ||
      entity.workloadUid !== undefined ||
      entity.podName !== undefined ||
      entity.podUid !== undefined ||
      entity.containerName !== undefined)
  ) {
    err(errors, `${path}.namespace`, "Workload/Pod/Container 신원에는 namespace가 있어야 합니다");
  }
  if (entity.serviceNamespace !== undefined && entity.serviceName === undefined) {
    err(errors, `${path}.serviceNamespace`, "serviceNamespace는 serviceName이 있어야 합니다");
  }
  if (entity.serviceVersion !== undefined && entity.serviceName === undefined) {
    err(errors, `${path}.serviceVersion`, "serviceVersion은 serviceName이 있어야 합니다");
  }
  return errors;
}

/**
 * exceedsCodePoints는 문자열이 max 유니코드 **코드포인트**를 넘는지 확인합니다.
 * JSON Schema maxLength·Go utf8.RuneCountInString과 같은 단위이며(UTF-16 유닛 아님),
 * max+1 코드포인트에서 멈추므로 문자열 길이와 무관하게 유계입니다.
 */
function exceedsCodePoints(s, max) {
  let n = 0;
  for (const _ of s) {
    n += 1;
    if (n > max) return true;
  }
  return false;
}

/** validateLabels는 라벨 수 n에 대해 O(n)으로 유계입니다. 상한 초과 시 개별 검사를 중단합니다. */
function validateLabels(labels, path) {
  const errors = [];
  if (labels === undefined) return errors;
  if (labels === null || typeof labels !== "object" || Array.isArray(labels)) {
    err(errors, path, "문자열 값의 객체여야 합니다");
    return errors;
  }
  // Object.keys 배열을 만들지 않고 own-key 순회로 세다가 상한+1에서 즉시 멈춥니다.
  let count = 0;
  for (const key in labels) {
    if (!Object.hasOwn(labels, key)) continue;
    count += 1;
    if (count > TELEMETRY_LABEL_LIMITS.maxCount) {
      err(errors, path, `라벨은 최대 ${TELEMETRY_LABEL_LIMITS.maxCount}개입니다`);
      return errors;
    }
  }
  for (const key in labels) {
    if (!Object.hasOwn(labels, key)) continue;
    if (key.length === 0 || exceedsCodePoints(key, TELEMETRY_LABEL_LIMITS.maxKeyLength)) {
      err(errors, `${path}.${key}`, `라벨 키는 1~${TELEMETRY_LABEL_LIMITS.maxKeyLength}자(코드포인트)입니다`);
      continue;
    }
    if (RESERVED_SET.has(key)) {
      err(errors, `${path}.${key}`, "신원·서비스·트레이스·메시지 필드는 라벨로 쓸 수 없습니다");
      continue;
    }
    const value = labels[key];
    if (typeof value !== "string") {
      err(errors, `${path}.${key}`, "라벨 값은 문자열이어야 합니다");
    } else if (exceedsCodePoints(value, TELEMETRY_LABEL_LIMITS.maxValueLength)) {
      err(errors, `${path}.${key}`, `라벨 값은 최대 ${TELEMETRY_LABEL_LIMITS.maxValueLength}자(코드포인트)입니다`);
    }
  }
  return errors;
}

function validateScope(scope, path) {
  const errors = [];
  if (scope === null || typeof scope !== "object" || Array.isArray(scope)) {
    err(errors, path, "객체여야 합니다");
    return errors;
  }
  if (scope.entity === undefined) {
    err(errors, `${path}.entity`, "entity는 필수입니다");
  } else {
    errors.push(...validateEntityRef(scope.entity, `${path}.entity`));
  }
  errors.push(...validateLabels(scope.labels, `${path}.labels`));
  optionalString(errors, `${path}.traceId`, scope.traceId);
  optionalString(errors, `${path}.spanId`, scope.spanId);
  return errors;
}

function validateTimestamp(errors, value) {
  if (!Number.isInteger(value) || value < 1) {
    err(errors, "timestampMs", "timestampMs는 1 이상의 epoch milliseconds 정수여야 합니다");
  }
}

function requireNonEmptyString(errors, path, value) {
  if (typeof value !== "string" || value === "") {
    err(errors, path, "비어 있지 않은 문자열이어야 합니다");
  }
}

/**
 * validateTelemetryRecord는 type으로 판별해 레코드 하나를 검사합니다.
 * 반환은 필드 경로가 붙은 오류 목록이며, 비어 있으면 유효합니다.
 * Go의 contract.ValidateTelemetryRecord와 규칙·경로가 같습니다.
 */
export function validateTelemetryRecord(record) {
  const errors = [];
  if (record === null || typeof record !== "object" || Array.isArray(record)) {
    err(errors, "", "객체여야 합니다");
    return errors;
  }
  if (!TELEMETRY_RECORD_TYPES.includes(record.type)) {
    err(errors, "type", `type은 ${TELEMETRY_RECORD_TYPES.join("|")} 중 하나여야 합니다`);
    return errors;
  }
  errors.push(...validateScope(record.scope, "scope"));
  validateTimestamp(errors, record.timestampMs);

  switch (record.type) {
    case "metric":
      requireNonEmptyString(errors, "name", record.name);
      if (!UNIT_SET.has(record.unit)) {
        err(errors, "unit", `unit은 ${METRIC_UNITS.join("|")} 중 하나여야 합니다`);
      }
      if (typeof record.value !== "number" || !Number.isFinite(record.value)) {
        err(errors, "value", "value는 유한한 숫자여야 합니다");
      }
      break;
    case "log":
      if (!LOG_LEVELS.includes(record.level)) {
        err(errors, "level", `level은 ${LOG_LEVELS.join("|")} 중 하나여야 합니다`);
      }
      if (typeof record.message !== "string") {
        err(errors, "message", "message는 필수 문자열입니다");
      }
      break;
    case "event":
      if (!EVENT_TYPES.includes(record.eventType)) {
        err(errors, "eventType", `eventType은 ${EVENT_TYPES.join("|")} 중 하나여야 합니다`);
      }
      requireNonEmptyString(errors, "reason", record.reason);
      if (record.message !== undefined && typeof record.message !== "string") {
        err(errors, "message", "message는 문자열이어야 합니다");
      }
      if (record.count !== undefined && (!Number.isInteger(record.count) || record.count < 1)) {
        err(errors, "count", "count는 1 이상의 정수여야 합니다");
      }
      break;
    case "alert":
      requireNonEmptyString(errors, "name", record.name);
      if (!ALERT_SEVERITIES.includes(record.severity)) {
        err(errors, "severity", `severity는 ${ALERT_SEVERITIES.join("|")} 중 하나여야 합니다`);
      }
      if (!ALERT_STATUSES.includes(record.status)) {
        err(errors, "status", `status는 ${ALERT_STATUSES.join("|")} 중 하나여야 합니다`);
      }
      break;
  }
  return errors;
}
