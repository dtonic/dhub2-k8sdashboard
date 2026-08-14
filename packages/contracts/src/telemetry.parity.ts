/**
 * 스키마 ↔ TypeScript 컴파일 타임 동등성 검사 — 이슈 #4
 * --------------------------------------------------------------------------
 * 정본 `schema/telemetry.schema.json`을 직접 import해 **속성 이름**을,
 * 거울 파일 `schema/telemetry.shape.json`으로 **필수 여부**를 타입 수준에서
 * 대조합니다. (JSON import는 배열 원소를 리터럴로 보존하지 않아 required
 * 배열 대신 키가 보존되는 shape 객체를 씁니다. shape ≡ schema 동등성은
 * `test/schema-parity.test.mjs`가 실행 시점에 검증합니다.)
 *
 * 스키마와 타입이 어긋나면 이 파일이 **컴파일 오류**로 실패합니다.
 * 앱 코드는 이 파일을 import하지 않습니다 — tsc 검사 전용입니다.
 */

import schema from "../schema/telemetry.schema.json";
import shape from "../schema/telemetry.shape.json";
import type { EntityRef } from "./index";
import type {
  AlertRecord,
  EventRecord,
  LogRecord,
  MetricRecord,
  MetricUnit,
  TelemetryScope,
} from "./telemetry";
import type { METRIC_UNITS, RESERVED_LABEL_KEYS } from "./telemetry.runtime.js";

type Equals<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
type Assert<T extends true> = T;

type RequiredKeys<T> = { [K in keyof T]-?: undefined extends T[K] ? never : K }[keyof T];
type OptionalKeys<T> = Exclude<keyof T, RequiredKeys<T>>;

type Defs = (typeof schema)["$defs"];
type Shape = typeof shape;

/** 스키마 $defs의 속성 이름 집합과 TS 타입의 키 집합이 정확히 같아야 합니다. */
type SchemaProps<D extends keyof Defs> = Defs[D] extends { properties: infer P } ? keyof P : never;
/** shape 거울의 required/optional 키 집합입니다. */
type ShapeRequired<D extends keyof Shape> = Shape[D] extends { required: infer R } ? keyof R : never;
type ShapeOptional<D extends keyof Shape> = Shape[D] extends { optional: infer O } ? keyof O : never;

type ParityOf<D extends keyof Defs & keyof Shape, T> = Equals<SchemaProps<D>, keyof T> extends true
  ? Equals<ShapeRequired<D>, RequiredKeys<T>> extends true
    ? Equals<ShapeOptional<D>, OptionalKeys<T>>
    : false
  : false;

export type EntityRefParity = Assert<ParityOf<"EntityRef", EntityRef>>;
export type TelemetryScopeParity = Assert<ParityOf<"TelemetryScope", TelemetryScope>>;
export type MetricRecordParity = Assert<ParityOf<"MetricRecord", MetricRecord>>;
export type LogRecordParity = Assert<ParityOf<"LogRecord", LogRecord>>;
export type EventRecordParity = Assert<ParityOf<"EventRecord", EventRecord>>;
export type AlertRecordParity = Assert<ParityOf<"AlertRecord", AlertRecord>>;

/** 런타임 상수 튜플 ↔ TS 유니언 동등성. 스키마 enum과의 대조는 mjs 테스트가 합니다. */
export type MetricUnitParity = Assert<Equals<(typeof METRIC_UNITS)[number], MetricUnit>>;

/** 예약 라벨 키 ↔ 스키마 Labels의 금지 속성(false) 집합 동등성입니다. */
export type ReservedLabelKeyParity = Assert<
  Equals<(typeof RESERVED_LABEL_KEYS)[number], keyof Defs["Labels"]["properties"]>
>;
