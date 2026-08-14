/**
 * CorrelationKey 우선순위·재생성 구분과 필드 경로 검증 동작 테스트입니다.
 * Go(apps/api/internal/contract/telemetry_test.go)와 같은 기대값을 씁니다.
 */
import { strict as assert } from "node:assert";
import test from "node:test";

import { correlationKey, validateEntityRef, validateTelemetryRecord } from "../src/telemetry.runtime.js";

const podEntity = {
  clusterId: "prod-seoul",
  namespace: "payments",
  workloadKind: "Deployment",
  workloadName: "payments-api",
  workloadUid: "wl-uid-1",
  podName: "payments-api-7f9c6b-x2k4q",
  podUid: "pod-uid-1",
};

test("CorrelationKey는 UID 우선순위를 따르고 클러스터 신원을 포함한다", () => {
  assert.equal(correlationKey(podEntity), "prod-seoul|pod-uid|pod-uid-1");
  assert.equal(
    correlationKey({ clusterId: "prod-seoul", workloadUid: "wl-uid-1" }),
    "prod-seoul|workload-uid|wl-uid-1",
  );
  assert.equal(
    correlationKey({
      clusterId: "prod-seoul", namespace: "payments",
      workloadKind: "Deployment", workloadName: "payments-api",
    }),
    "prod-seoul|workload|payments|Deployment|payments-api",
  );
  assert.equal(
    correlationKey({ clusterId: "prod-seoul", namespace: "payments", podName: "adhoc-pod", podUid: "" }),
    "prod-seoul|pod-name|payments|adhoc-pod",
  );
  // 클러스터가 다르면 같은 UID라도 다른 키입니다.
  assert.notEqual(correlationKey(podEntity), correlationKey({ ...podEntity, clusterId: "stage-seoul" }));
});

test("같은 Pod의 metric/log/event는 같은 키로 상관된다", () => {
  const ts = 1765689600000;
  const scope = { entity: podEntity };
  const records = [
    { type: "metric", scope, name: "container_cpu_usage", unit: "millicores", timestampMs: ts, value: 1 },
    { type: "log", scope, timestampMs: ts, level: "ERROR", message: "실패" },
    { type: "event", scope, timestampMs: ts, eventType: "Warning", reason: "BackOff" },
  ];
  const keys = new Set(records.map((r) => correlationKey(r.scope.entity)));
  assert.equal(keys.size, 1);
  for (const r of records) assert.deepEqual(validateTelemetryRecord(r), []);
});

test("이름이 같아도 Pod UID가 다르면 (재생성) 다른 키가 된다", () => {
  const recreated = { ...podEntity, podUid: "pod-uid-2" };
  assert.equal(podEntity.podName, recreated.podName);
  assert.notEqual(correlationKey(podEntity), correlationKey(recreated));
});

test("EntityRef는 README §5 계층 불변식을 강제한다", () => {
  const paths = (entity) => validateEntityRef(entity, "entity").map((e) => e.path);

  // 거부: kind-only / name-only (fallback 신원은 ns+kind+name 삼중쌍)
  assert.deepEqual(paths({ clusterId: "c1", namespace: "payments", workloadKind: "Deployment" }), ["entity.workloadName"]);
  assert.deepEqual(paths({ clusterId: "c1", namespace: "payments", workloadName: "payments-api" }), ["entity.workloadKind"]);
  // 거부: namespace 없는 Workload/Pod/Container 신원 (namespace 오류는 중복 없이 1건)
  assert.deepEqual(paths({ clusterId: "c1", workloadKind: "Deployment", workloadName: "payments-api" }), ["entity.namespace"]);
  assert.deepEqual(paths({ clusterId: "c1", workloadUid: "wl-1" }), ["entity.namespace"]);
  assert.deepEqual(paths({ clusterId: "c1", podName: "p-1", podUid: "u-1" }), ["entity.namespace"]);
  assert.deepEqual(paths({ clusterId: "c1", podName: "p-1", podUid: "u-1", containerName: "app" }), ["entity.namespace"]);
  // 허용: cluster-only, namespace-only, workloadUid+namespace (UID-only도 namespace는 필요)
  assert.deepEqual(paths({ clusterId: "c1" }), []);
  assert.deepEqual(paths({ clusterId: "c1", namespace: "payments" }), []);
  assert.deepEqual(paths({ clusterId: "c1", namespace: "payments", workloadUid: "wl-1" }), []);
});

test("검증 오류에는 명확한 필드 경로가 붙는다", () => {
  const paths = (record) => validateTelemetryRecord(record).map((e) => e.path);
  assert.deepEqual(
    paths({ type: "metric", scope: { entity: { clusterId: "c1", podUid: "u1" } }, name: "m", unit: "cores", timestampMs: 1, value: 0 }),
    ["scope.entity.podName", "scope.entity.namespace"],
  );
  assert.deepEqual(
    paths({ type: "log", scope: { entity: { clusterId: "c1", serviceNamespace: "payments" } }, timestampMs: 1, level: "INFO", message: "" }),
    ["scope.entity.serviceNamespace"],
  );
  assert.deepEqual(
    paths({ type: "alert", scope: { entity: { clusterId: "c1" }, labels: { message: "x" } }, timestampMs: 0, name: "a", severity: "critical", status: "firing" }),
    ["scope.labels.message", "timestampMs"],
  );
  // EntityRef로 흡수되는 원본 속성 별칭(service.name, k8s.pod.uid 등)도
  // 라벨로 밀반입할 수 없고, 항목 필드 경로가 붙습니다.
  assert.deepEqual(
    paths({
      type: "log",
      scope: { entity: { clusterId: "c1" }, labels: { "service.name": "payments-api", "k8s.pod.uid": "u1" } },
      timestampMs: 1, level: "INFO", message: "ok",
    }),
    ["scope.labels.service.name", "scope.labels.k8s.pod.uid"],
  );
  // 라벨 상한 초과는 항목별 검사 없이 상한 오류 하나로 유계 처리됩니다 (O(n)).
  const labels = Object.fromEntries(Array.from({ length: 40 }, (_, i) => [`k${i}`, "v"]));
  assert.deepEqual(
    paths({ type: "event", scope: { entity: { clusterId: "c1" }, labels }, timestampMs: 1, eventType: "Normal", reason: "Scheduled" }),
    ["scope.labels"],
  );
});
