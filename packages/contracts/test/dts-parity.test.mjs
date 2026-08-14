/**
 * telemetry.runtime.d.ts(수기 타입 선언) ↔ telemetry.runtime.js(실제 값) 드리프트 검사입니다.
 *
 * TS의 MetricUnit 등은 d.ts의 리터럴 튜플에서 유도되므로, d.ts가 js와 어긋나면
 * 컴파일 타임 파리티(telemetry.parity.ts)가 거짓 통과할 수 있습니다. 여기서
 * d.ts 본문에서 선언된 리터럴을 추출해 js 값과 기계 대조합니다 (표준 라이브러리만 사용).
 */
import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import test from "node:test";

import * as runtime from "../src/telemetry.runtime.js";

const dts = readFileSync(new URL("../src/telemetry.runtime.d.ts", import.meta.url), "utf8");

/** d.ts에서 `export declare const <name>: readonly [ ... ]` 튜플의 문자열 리터럴을 뽑습니다. */
function declaredTuple(name) {
  const m = dts.match(new RegExp(`export declare const ${name}: readonly \\[([\\s\\S]*?)\\];`));
  assert.ok(m, `d.ts에서 ${name} 튜플 선언을 찾지 못했습니다`);
  return [...m[1].matchAll(/"([^"]*)"/g)].map((x) => x[1]);
}

test("d.ts의 리터럴 튜플은 runtime.js 값과 같다", () => {
  for (const name of ["METRIC_UNITS", "RESERVED_LABEL_KEYS", "WORKLOAD_KINDS", "TELEMETRY_RECORD_TYPES"]) {
    assert.deepEqual(declaredTuple(name), [...runtime[name]], `${name} 선언과 값이 다릅니다`);
  }
});

test("d.ts의 라벨 한도 리터럴은 runtime.js 값과 같다", () => {
  const m = dts.match(/export declare const TELEMETRY_LABEL_LIMITS: \{([\s\S]*?)\};/);
  assert.ok(m, "d.ts에서 TELEMETRY_LABEL_LIMITS 선언을 찾지 못했습니다");
  const declared = Object.fromEntries(
    [...m[1].matchAll(/readonly (\w+): (\d+);/g)].map((x) => [x[1], Number(x[2])]),
  );
  assert.deepEqual(declared, { ...runtime.TELEMETRY_LABEL_LIMITS });
});

test("d.ts가 선언한 함수·타입 이름이 runtime.js에 실제로 있다", () => {
  for (const name of ["correlationKey", "validateEntityRef", "validateTelemetryRecord"]) {
    assert.ok(dts.includes(`export declare function ${name}(`), `d.ts에 ${name} 선언이 없습니다`);
    assert.equal(typeof runtime[name], "function", `runtime.js에 ${name}가 없습니다`);
  }
});
