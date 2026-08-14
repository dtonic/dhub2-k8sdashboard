/**
 * telemetry.runtime.js의 타입 선언입니다. 리터럴 튜플로 선언해
 * telemetry.parity.ts의 컴파일 타임 동등성 검사에 쓰입니다.
 * 값과 선언이 일치하는지는 test/*.mjs와 tsc가 함께 검증합니다.
 */
import type { EntityRef } from "./index";

export declare const METRIC_UNITS: readonly [
  "percent",
  "bytes",
  "bytes_per_sec",
  "count",
  "cores",
  "millicores",
  "mebibytes",
];

export declare const TELEMETRY_LABEL_LIMITS: {
  readonly maxCount: 32;
  readonly maxKeyLength: 64;
  readonly maxValueLength: 256;
};

export declare const RESERVED_LABEL_KEYS: readonly [
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
];

export declare const WORKLOAD_KINDS: readonly [
  "Deployment",
  "StatefulSet",
  "DaemonSet",
  "ReplicaSet",
  "CronJob",
];

export declare const TELEMETRY_RECORD_TYPES: readonly ["metric", "log", "event", "alert"];

/** 검증 오류 하나. path는 "scope.entity.podUid" 같은 필드 경로입니다. */
export interface TelemetryFieldError {
  path: string;
  message: string;
}

export declare function correlationKey(entity: EntityRef): string;
export declare function validateEntityRef(entity: unknown, path?: string): TelemetryFieldError[];
export declare function validateTelemetryRecord(record: unknown): TelemetryFieldError[];
