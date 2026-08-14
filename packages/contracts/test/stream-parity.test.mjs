/**
 * SSE 봉투 파리티 — 정본 스키마(schema/stream.schema.json) ↔ TS(src/index.ts). (이슈 #12)
 *
 * index.ts는 손으로 유지하는 원본이므로, dts-parity와 같은 방식으로 소스 텍스트에서
 * 선언을 추출해 스키마와 기계 대조합니다 (표준 라이브러리만 사용).
 * Go 쪽 동등성은 apps/api/internal/contract/stream_parity_test.go가 증명합니다.
 */
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import test from "node:test";

const schema = JSON.parse(readFileSync(new URL("../schema/stream.schema.json", import.meta.url), "utf8"));
const src = readFileSync(new URL("../src/index.ts", import.meta.url), "utf8");

/** index.ts에서 `export type <name> = "a" | "b";` union의 문자열 리터럴을 뽑습니다. */
function declaredUnion(name) {
  const m = src.match(new RegExp(`export type ${name} =([^;]*);`));
  assert.ok(m, `index.ts에서 ${name} union 선언을 찾지 못했습니다`);
  return [...m[1].matchAll(/"([^"]*)"/g)].map((x) => x[1]);
}

/** index.ts에서 interface 본문의 (필드 이름 → required 여부)를 뽑습니다. */
function declaredInterface(name) {
  const m = src.match(new RegExp(`export interface ${name} \\{([\\s\\S]*?)\\n\\}`));
  assert.ok(m, `index.ts에서 ${name} interface 선언을 찾지 못했습니다`);
  const fields = {};
  for (const line of m[1].split("\n")) {
    const f = line.match(/^\s{2}(\w+)(\?)?:/);
    if (f) fields[f[1]] = !f[2];
  }
  return fields;
}

test("StreamEventKind/StreamEventAction union은 스키마 enum과 같다", () => {
  assert.deepEqual(declaredUnion("StreamEventKind"), schema.$defs.StreamEventKind.enum);
  assert.deepEqual(declaredUnion("StreamEventAction"), schema.$defs.StreamEventAction.enum);
});

test("EventEnvelope 필드 이름과 필수 여부는 스키마와 같다", () => {
  const fields = declaredInterface("EventEnvelope");
  const def = schema.$defs.EventEnvelope;
  assert.deepEqual(Object.keys(fields).sort(), Object.keys(def.properties).sort(), "속성 이름 불일치");
  const tsRequired = Object.keys(fields).filter((k) => fields[k]).sort();
  assert.deepEqual(tsRequired, [...def.required].sort(), "필수 필드 불일치");
});

test("EventEnvelope.entity는 telemetry 정본 EntityRef를 참조한다", () => {
  assert.equal(schema.$defs.EventEnvelope.properties.entity.$ref, "./telemetry.schema.json#/$defs/EntityRef");
  assert.match(src, /entity\?: EntityRef;/);
});
