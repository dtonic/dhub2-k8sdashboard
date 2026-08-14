/**
 * Unified Telemetry Model — 이슈 #4
 * --------------------------------------------------------------------------
 * 기계가 읽는 정본은 `schema/telemetry.schema.json`(JSON Schema Draft 2020-12)입니다.
 * 이 파일의 타입과 스키마의 동등성(속성 이름·필수 여부)은 `telemetry.parity.ts`가
 * 컴파일 타임에, 상수·검증 동작은 `test/*.mjs`가 실행 시점에 증명합니다.
 *
 * 규칙 요약
 * - 시각은 **epoch milliseconds**(`timestampMs`)입니다. 기존 화면 DTO의 RFC3339는
 *   그대로 두고, 새 정본 텔레메트리 계약에만 적용합니다.
 * - 라벨은 최대 32개, 키 64자, 값 256자. 신원·서비스·트레이스·메시지 필드는
 *   라벨로 넣지 않습니다(RESERVED_LABEL_KEYS).
 * - 상관키는 UID 우선(README §5)이며 클러스터 신원을 항상 포함합니다.
 */

import type { EntityRef } from "./index";
import type { METRIC_UNITS } from "./telemetry.runtime.js";

export {
  METRIC_UNITS,
  TELEMETRY_LABEL_LIMITS,
  RESERVED_LABEL_KEYS,
  WORKLOAD_KINDS,
  TELEMETRY_RECORD_TYPES,
  correlationKey,
  validateEntityRef,
  validateTelemetryRecord,
} from "./telemetry.runtime.js";
export type { TelemetryFieldError } from "./telemetry.runtime.js";

/** Query Catalog가 실제로 쓰는 모든 단위를 포함하는 통합 단위입니다. */
export type MetricUnit = (typeof METRIC_UNITS)[number];

/** 부가 라벨. 한도는 TELEMETRY_LABEL_LIMITS, 금지 키는 RESERVED_LABEL_KEYS를 따릅니다. */
export type TelemetryLabels = Record<string, string>;

/** 모든 텔레메트리 레코드가 공유하는 관측 범위입니다. */
export interface TelemetryScope {
  entity: EntityRef;
  labels?: TelemetryLabels;
  traceId?: string;
  spanId?: string;
}

export interface MetricRecord {
  type: "metric";
  scope: TelemetryScope;
  name: string;
  unit: MetricUnit;
  /** epoch milliseconds */
  timestampMs: number;
  value: number;
}

export interface LogRecord {
  type: "log";
  scope: TelemetryScope;
  /** epoch milliseconds */
  timestampMs: number;
  level: "ERROR" | "WARN" | "INFO" | "DEBUG";
  message: string;
}

export interface EventRecord {
  type: "event";
  scope: TelemetryScope;
  /** epoch milliseconds */
  timestampMs: number;
  eventType: "Normal" | "Warning";
  reason: string;
  message?: string;
  count?: number;
}

export interface AlertRecord {
  type: "alert";
  scope: TelemetryScope;
  /** epoch milliseconds */
  timestampMs: number;
  name: string;
  severity: "critical" | "warning" | "info";
  status: "firing" | "resolved" | "pending";
}

/** type 필드로 판별되는 discriminated union입니다. */
export type TelemetryRecord = MetricRecord | LogRecord | EventRecord | AlertRecord;
