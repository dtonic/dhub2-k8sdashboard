/**
 * 스키마 정본 ↔ shape 거울 ↔ 런타임 상수의 동등성과, 예시·오류 페이로드의
 * 스키마 검증을 Node 표준 라이브러리만으로 수행합니다 (외부 의존성·코드젠 없음).
 *
 * 검증 체인:
 *   telemetry.schema.json (정본)
 *     ≡ telemetry.shape.json  → tsc(telemetry.parity.ts)가 TS 타입과 대조
 *     ≡ telemetry.runtime.js 상수 → 런타임 검증기가 같은 규칙을 강제
 */
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  METRIC_UNITS,
  RESERVED_LABEL_KEYS,
  TELEMETRY_LABEL_LIMITS,
  validateTelemetryRecord,
} from "../src/telemetry.runtime.js";

const schema = JSON.parse(readFileSync(new URL("../schema/telemetry.schema.json", import.meta.url), "utf8"));
const shape = JSON.parse(readFileSync(new URL("../schema/telemetry.shape.json", import.meta.url), "utf8"));
const example = JSON.parse(readFileSync(new URL("../schema/telemetry.example.json", import.meta.url), "utf8"));

/* ── 최소 JSON Schema(Draft 2020-12 부분집합) 평가기 ─────────────────────── */
// 이 스키마가 실제로 쓰는 키워드만 지원합니다. 지원하지 않는 키워드가
// 스키마에 등장하면 조용히 통과시키지 않고 실패시킵니다.
const SUPPORTED = new Set([
  "$schema", "$id", "$ref", "$defs", "$comment", "title", "description",
  "type", "const", "enum", "required", "properties", "additionalProperties",
  "propertyNames", "maxProperties", "minLength", "maxLength", "minimum",
  "dependentRequired", "oneOf",
]);

function resolveRef(ref) {
  assert.match(ref, /^#\/\$defs\//, `지원하지 않는 $ref: ${ref}`);
  const name = ref.slice("#/$defs/".length);
  assert.ok(schema.$defs[name], `$defs에 없는 참조: ${ref}`);
  return schema.$defs[name];
}

function typeMatches(type, value) {
  switch (type) {
    case "object": return value !== null && typeof value === "object" && !Array.isArray(value);
    case "string": return typeof value === "string";
    case "number": return typeof value === "number";
    case "integer": return Number.isInteger(value);
    default: assert.fail(`지원하지 않는 type: ${type}`);
  }
}

function evaluate(node, value, path) {
  if (node === false) return [{ path, message: "허용되지 않습니다" }];
  if (node === true) return [];
  for (const key of Object.keys(node)) {
    assert.ok(SUPPORTED.has(key), `평가기가 지원하지 않는 키워드: ${key} (at ${path})`);
  }
  if (node.$ref) return evaluate(resolveRef(node.$ref), value, path);

  const errors = [];
  if (node.oneOf) {
    const passed = node.oneOf.filter((sub) => evaluate(sub, value, path).length === 0);
    if (passed.length !== 1) {
      errors.push({ path, message: `oneOf: ${passed.length}개 분기가 일치합니다 (정확히 1개여야 함)` });
    }
    return errors;
  }
  if (node.const !== undefined && value !== node.const) {
    return [{ path, message: `const ${node.const} 불일치` }];
  }
  if (node.enum && !node.enum.includes(value)) {
    return [{ path, message: "enum에 없는 값" }];
  }
  if (node.type && !typeMatches(node.type, value)) {
    return [{ path, message: `type ${node.type} 불일치` }];
  }
  if (typeof value === "string") {
    // JSON Schema의 min/maxLength는 유니코드 코드포인트 단위입니다 (UTF-16 유닛 아님).
    const cpLen = [...value].length;
    if (node.minLength !== undefined && cpLen < node.minLength) errors.push({ path, message: "minLength 위반" });
    if (node.maxLength !== undefined && cpLen > node.maxLength) errors.push({ path, message: "maxLength 위반" });
  }
  if (typeof value === "number" && node.minimum !== undefined && value < node.minimum) {
    errors.push({ path, message: "minimum 위반" });
  }
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    const keys = Object.keys(value);
    for (const req of node.required ?? []) {
      if (!(req in value)) errors.push({ path: `${path}.${req}`, message: "required 위반" });
    }
    for (const [trigger, deps] of Object.entries(node.dependentRequired ?? {})) {
      if (trigger in value) {
        for (const dep of deps) {
          if (!(dep in value)) errors.push({ path: `${path}.${dep}`, message: `dependentRequired(${trigger}) 위반` });
        }
      }
    }
    if (node.maxProperties !== undefined && keys.length > node.maxProperties) {
      errors.push({ path, message: "maxProperties 위반" });
    }
    for (const key of keys) {
      if (node.propertyNames) {
        errors.push(...evaluate(node.propertyNames, key, `${path}.${key}(키)`));
      }
      const sub = node.properties?.[key] !== undefined ? node.properties[key] : node.additionalProperties;
      if (sub !== undefined) errors.push(...evaluate(sub, value[key], `${path}.${key}`));
    }
  }
  return errors;
}

const validPerSchema = (v) => evaluate(schema, v, "$");

/* ── 정본·거울·런타임 상수 동등성 ────────────────────────────────────────── */

test("스키마는 Draft 2020-12를 선언한다", () => {
  assert.equal(schema.$schema, "https://json-schema.org/draft/2020-12/schema");
});

test("shape 거울은 스키마의 속성 이름·필수 여부와 정확히 같다", () => {
  const defs = Object.keys(shape).filter((k) => k !== "$comment");
  assert.deepEqual(
    defs.sort(),
    ["AlertRecord", "EntityRef", "EventRecord", "LogRecord", "MetricRecord", "TelemetryScope"],
  );
  for (const def of defs) {
    const s = schema.$defs[def];
    assert.ok(s, `${def}가 스키마 $defs에 없습니다`);
    const required = Object.keys(shape[def].required).sort();
    const optional = Object.keys(shape[def].optional).sort();
    assert.deepEqual(required, [...s.required].sort(), `${def}.required 불일치`);
    assert.deepEqual(
      [...required, ...optional].sort(),
      Object.keys(s.properties).sort(),
      `${def} 속성 이름 불일치`,
    );
    assert.equal(required.filter((k) => optional.includes(k)).length, 0, `${def} required∩optional ≠ ∅`);
  }
});

test("런타임 상수는 스키마와 같다 (MetricUnit, 예약 라벨 키, 라벨 한도)", () => {
  assert.deepEqual([...METRIC_UNITS], schema.$defs.MetricUnit.enum);
  const labels = schema.$defs.Labels;
  assert.deepEqual([...RESERVED_LABEL_KEYS].sort(), Object.keys(labels.properties).sort());
  for (const [key, sub] of Object.entries(labels.properties)) {
    assert.equal(sub, false, `예약 키 ${key}는 스키마에서 false여야 합니다`);
  }
  assert.equal(TELEMETRY_LABEL_LIMITS.maxCount, labels.maxProperties);
  assert.equal(TELEMETRY_LABEL_LIMITS.maxKeyLength, labels.propertyNames.maxLength);
  assert.equal(TELEMETRY_LABEL_LIMITS.maxValueLength, labels.additionalProperties.maxLength);
});

test("Query Catalog가 쓰는 모든 unit이 MetricUnit에 포함된다", () => {
  const yaml = readFileSync(
    new URL("../../../apps/api/internal/querycatalog/defaults/metrics.yaml", import.meta.url),
    "utf8",
  );
  const units = [...new Set([...yaml.matchAll(/^\s*unit:\s*(\S+)\s*$/gm)].map((m) => m[1]))];
  assert.ok(units.length > 0, "카탈로그에서 unit을 하나도 찾지 못했습니다");
  for (const u of units) {
    assert.ok(METRIC_UNITS.includes(u), `카탈로그 unit '${u}'가 MetricUnit에 없습니다`);
  }
});

/* ── 예시·오류 페이로드 검증 ─────────────────────────────────────────────── */

test("대표 예시 페이로드는 스키마와 런타임 검증기를 모두 통과한다", () => {
  assert.equal(example.length, 4);
  assert.deepEqual(example.map((r) => r.type), ["metric", "log", "event", "alert"]);
  for (const record of example) {
    assert.deepEqual(validPerSchema(record), [], `스키마 검증 실패: ${record.type}`);
    assert.deepEqual(validateTelemetryRecord(record), [], `런타임 검증 실패: ${record.type}`);
  }
});

test("필수 필드 누락·규칙 위반은 스키마와 런타임 검증기가 모두 거부한다", () => {
  const base = example[0];
  const invalid = [
    // 필수 누락
    { ...base, unit: undefined },
    { ...base, scope: undefined },
    (() => { const { value, ...rest } = base; void value; return rest; })(),
    // 판별자·enum 위반
    { ...base, type: "gauge" },
    { ...base, unit: "gigabytes" },
    // 시각 규칙: epoch ms 정수
    { ...base, timestampMs: 1765689600.5 },
    { ...base, timestampMs: "2026-08-14T00:00:00Z" },
    // Pod 신원 정합성: 이름·UID는 함께
    { ...base, scope: { entity: { clusterId: "c1", namespace: "payments", podName: "only-name" } } },
    // README §5 계층: kind-only / name-only, namespace 없는 workload/pod 신원
    { ...base, scope: { entity: { clusterId: "c1", namespace: "payments", workloadKind: "Deployment" } } },
    { ...base, scope: { entity: { clusterId: "c1", namespace: "payments", workloadName: "payments-api" } } },
    { ...base, scope: { entity: { clusterId: "c1", workloadUid: "wl-1" } } },
    { ...base, scope: { entity: { clusterId: "c1", podName: "p-1", podUid: "u-1" } } },
    // container는 Pod 신원 필요, service namespace/version은 serviceName 필요
    { ...base, scope: { entity: { clusterId: "c1", containerName: "app" } } },
    { ...base, scope: { entity: { clusterId: "c1", serviceVersion: "1.0.0" } } },
    // 라벨 한도·예약 키
    { ...base, scope: { ...base.scope, labels: Object.fromEntries(Array.from({ length: 33 }, (_, i) => [`k${i}`, "v"]) ) } },
    { ...base, scope: { ...base.scope, labels: { ["k".repeat(65)]: "v" } } },
    { ...base, scope: { ...base.scope, labels: { zone: "v".repeat(257) } } },
    { ...base, scope: { ...base.scope, labels: { podUid: "smuggled-identity" } } },
  ].map((r) => JSON.parse(JSON.stringify(r)));

  for (const [i, record] of invalid.entries()) {
    assert.notDeepEqual(validPerSchema(record), [], `스키마가 잘못된 페이로드 #${i}를 통과시켰습니다`);
    assert.notDeepEqual(validateTelemetryRecord(record), [], `런타임이 잘못된 페이로드 #${i}를 통과시켰습니다`);
  }
});

test("라벨 길이는 스키마·런타임 모두 유니코드 코드포인트로 센다", () => {
  // 이모지(😀)는 UTF-16 2유닛·UTF-8 4바이트, 한글(한)은 UTF-16 1유닛·UTF-8 3바이트입니다.
  // 코드포인트로 세지 않으면 스키마와 런타임의 경계 판정이 갈립니다.
  // Go(telemetry_test.go)와 같은 경계값을 씁니다.
  const withLabels = (labels) => JSON.parse(JSON.stringify({ ...example[0], scope: { ...example[0].scope, labels } }));
  const cases = [
    { labels: { ["😀".repeat(64)]: "v" }, valid: true },
    { labels: { ["😀".repeat(65)]: "v" }, valid: false },
    { labels: { zone: "한".repeat(256) }, valid: true },
    { labels: { zone: "한".repeat(257) }, valid: false },
  ];
  for (const [i, c] of cases.entries()) {
    const record = withLabels(c.labels);
    const schemaErrs = validPerSchema(record);
    const runtimeErrs = validateTelemetryRecord(record);
    if (c.valid) {
      assert.deepEqual(schemaErrs, [], `스키마가 경계 내 라벨 #${i}를 거부했습니다`);
      assert.deepEqual(runtimeErrs, [], `런타임이 경계 내 라벨 #${i}를 거부했습니다`);
    } else {
      assert.notDeepEqual(schemaErrs, [], `스키마가 경계 초과 라벨 #${i}를 통과시켰습니다`);
      assert.notDeepEqual(runtimeErrs, [], `런타임이 경계 초과 라벨 #${i}를 통과시켰습니다`);
    }
  }
});

test("cluster-only, namespace-only, workloadUid+namespace 엔티티는 유효하다", () => {
  const entities = [
    { clusterId: "c1" },
    { clusterId: "c1", namespace: "payments" },
    { clusterId: "c1", namespace: "payments", workloadUid: "wl-1" },
  ];
  for (const entity of entities) {
    const record = { type: "log", scope: { entity }, timestampMs: 1, level: "INFO", message: "ok" };
    assert.deepEqual(validPerSchema(record), [], `스키마가 유효한 엔티티를 거부했습니다: ${JSON.stringify(entity)}`);
    assert.deepEqual(validateTelemetryRecord(record), [], `런타임이 유효한 엔티티를 거부했습니다: ${JSON.stringify(entity)}`);
  }
});

test("value=0과 message=\"\"는 유효하고, 필드 부재는 거부된다", () => {
  const scope = { entity: { clusterId: "c1" } };
  const zeroValue = { type: "metric", scope, name: "m", unit: "cores", timestampMs: 1, value: 0 };
  const emptyMessage = { type: "log", scope, timestampMs: 1, level: "INFO", message: "" };
  for (const r of [zeroValue, emptyMessage]) {
    assert.deepEqual(validPerSchema(r), []);
    assert.deepEqual(validateTelemetryRecord(r), []);
  }
  const { value, ...noValue } = zeroValue;
  void value;
  const { message, ...noMessage } = emptyMessage;
  void message;
  for (const r of [noValue, noMessage]) {
    assert.notDeepEqual(validPerSchema(r), []);
    assert.notDeepEqual(validateTelemetryRecord(r), []);
  }
});
