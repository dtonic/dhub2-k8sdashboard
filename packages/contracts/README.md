# @k8s-dashboard/contracts

Observability API/BFF(Go)와 Web UI(TypeScript)가 공유하는 계약입니다.
화면 응답 계약은 `src/index.ts`, **Unified Telemetry Model(이슈 #4)**은 아래 구조를 따릅니다.

## 정본과 매핑

기계가 읽는 단일 정본(SSoT)은 **`schema/telemetry.schema.json`** (JSON Schema Draft 2020-12)입니다.
TS와 Go는 이 스키마와의 동등성을 코드젠 없이 **실행 가능한 검사**로 증명합니다.

| 계층 | 파일 | 동등성 증명 |
|---|---|---|
| 정본 스키마 | `schema/telemetry.schema.json` | — |
| 대표 예시 | `schema/telemetry.example.json` | `test/schema-parity.test.mjs`가 스키마·런타임 검증기로 통과 확인 |
| shape 거울 | `schema/telemetry.shape.json` | `test/schema-parity.test.mjs`가 스키마와 기계 대조 |
| TS 타입 | `src/telemetry.ts`, `src/index.ts`(EntityRef) | `src/telemetry.parity.ts`가 **컴파일 타임**에 속성 이름·필수 여부 대조 (`npm run check`) |
| TS/JS 런타임 | `src/telemetry.runtime.js`(+`.d.ts`) | `npm run test` (`node --test`, 표준 라이브러리만). `test/dts-parity.test.mjs`가 d.ts 리터럴 ↔ js 값 드리프트도 대조 |
| Go 타입 | `apps/api/internal/contract/telemetry.go` | `telemetry_parity_test.go`가 reflection으로 대조 (`go test`) |

계약을 바꿀 때는 **스키마를 먼저** 고치고 shape 거울 → TS → Go 순서로 맞춥니다.
어긋나면 tsc·node --test·go test 중 하나가 반드시 실패합니다.

## 원본 속성 → EntityRef 매핑 (정본)

데이터소스가 내려주는 아래 원본 속성은 **EntityRef 필드로 흡수(consume)**됩니다.
이 속성들은 임의 라벨로 남겨 두지 않습니다 — 정규화된 이름과 원본 별칭 모두
`RESERVED_LABEL_KEYS`로 예약되어 검증이 거부합니다.

| 원본 속성 (source attribute) | EntityRef 필드 |
|---|---|
| `cluster.id` | `clusterId` |
| `k8s.namespace.name` | `namespace` |
| `k8s.workload.kind` | `workloadKind` |
| `k8s.workload.name` | `workloadName` |
| `k8s.workload.uid` | `workloadUid` |
| `k8s.pod.name` | `podName` |
| `k8s.pod.uid` | `podUid` |
| `k8s.container.name` | `containerName` |
| `service.name` | `serviceName` |
| `service.namespace` | `serviceNamespace` |
| `service.version` | `serviceVersion` |

트레이스 문맥(`trace_id`/`span_id`)도 같은 원칙으로 `TelemetryScope.traceId`/`spanId`로
흡수되며 라벨로 쓸 수 없습니다. `clusterName`·`nodeName`은 이 이슈의 매핑 범위가 아닙니다.

## 규칙

- **시각**: 정본 텔레메트리 레코드는 `timestampMs`(epoch milliseconds, 정수 ≥1)만 씁니다.
  기존 화면 DTO의 RFC3339 문자열은 이 규칙의 대상이 아닙니다.
- **단위**: `MetricUnit`은 Query Catalog(`querycatalog/defaults/*.yaml`)가 실제로 쓰는
  모든 단위(`percent | bytes | bytes_per_sec | count | cores | millicores | mebibytes`)를 포함하며,
  카탈로그 단위 ⊆ MetricUnit을 테스트가 강제합니다.
- **라벨 상한**: 최대 32개, 키 1~64자, 값 최대 256자. 길이는 JSON Schema `maxLength`와
  같은 **유니코드 코드포인트** 단위로 셉니다(TS는 코드포인트 순회, Go는
  `utf8.RuneCountInString` — UTF-16 유닛·바이트 아님). 검증은 라벨 수 n에 대해 O(n)으로
  유계이며, 상한 초과 시 항목별 검사 없이 상한 오류 하나로 끝납니다.
- **부재 ≠ 제로**: 스키마 `required`는 필드 존재를 뜻합니다. 0이 유효한 `MetricRecord.value`와
  빈 문자열이 유효한 `LogRecord.message`는 Go에서 포인터(`*float64`, `*string`)로 부재를
  구분합니다. `value: 0`, `message: ""`는 유효하고, 필드 부재는 세 계층 모두 거부합니다.
- **예약 키**: 신원·서비스·트레이스·메시지 필드(`podUid`, `serviceName`, `traceId`, `message` 등
  `RESERVED_LABEL_KEYS`)는 임의 라벨로 넣을 수 없습니다. 스키마에서는 `Labels.properties.<key>: false`로 금지합니다.
- **신원 정합성 (README §5 계층)**: `Cluster → Namespace → Workload → Pod → Container`
  계층을 검증이 강제합니다.
  - Workload/Pod/Container 신원이 하나라도 있으면 `namespace`가 필요합니다
    (namespace 오류는 중복 없이 1건만 보고).
  - fallback 신원은 `namespace + workloadKind + workloadName` 삼중쌍입니다 —
    kind-only는 `workloadName` 경로로, name-only는 `workloadKind` 경로로 거부됩니다.
  - `workloadUid`만 있는 워크로드 참조는 kind/name 없이 **유효하지만 `namespace`는 필요**합니다.
    UID는 전역 유일해도, 계층상 namespace 없는 워크로드 신원은 화면 경로(deep link)를
    만들 수 없기 때문입니다.
  - Pod 수준 레코드는 `podName`·`podUid`가 **함께** 있어야 합니다. `containerName`은
    Pod 신원이, `serviceNamespace`/`serviceVersion`은 `serviceName`이 있어야 합니다.
  - `correlationKey`는 검증 전·레거시의 부분 참조도 접을 수 있도록 이 불변식을
    강제하지 않습니다(동작 불변).
- **상관키**: `correlationKey`(TS) / `contract.CorrelationKey`(Go)는 UID 우선순위
  (Pod UID → Workload UID → ns+kind+name → Pod Name, README §5)로 접고 클러스터 신원을 항상
  포함합니다. 같은 Pod의 metric/log/event는 같은 키, 이름이 같아도 UID가 다른(재생성된)
  Pod는 다른 키가 됩니다.
- **검증 오류**: `scope.entity.podUid` 같은 필드 경로가 붙습니다. TS와 Go의 규칙·경로는 동일합니다.

## 검증 명령

```sh
npm run check --workspace @k8s-dashboard/contracts   # tsc 컴파일 타임 파리티
npm run test  --workspace @k8s-dashboard/contracts   # node --test (스키마·예시·상관키)
cd apps/api && go test ./internal/contract/          # Go reflection 파리티·동작
```
