# Dashboard DSL

`schema/dashboard.schema.json`이 schemaVersion 1의 구조 정본입니다. TypeScript 타입과 런타임 검증기는 동일한 parity corpus로 검사합니다. 중복 ID, 고정-grid 겹침, query catalog membership처럼 표준 keyword만으로 표현하기 어려운 제약은 schema의 `x-dashboard-semantics` keyword와 런타임 semantic validation이 독립 구현으로 같은 corpus를 검사합니다. 외부 JSON Schema validator는 이 keyword를 fail-open으로 무시하지 말고 등록해야 합니다.

## 추가 절차와 제한

`dashboards/`에 일반 `.json` 파일 하나를 커밋하면 `npm run generate`가 자동 발견하여 navigation과 `/dashboards/:id`에 노출합니다. 하위 디렉터리, symlink, 비정규 파일, 경로 구분자, 파일당 64 KiB, 32개 파일, dashboard당 widget 24개와 variable 2개를 허용하지 않습니다. layout은 12열, 96행의 겹치지 않는 정수 좌표입니다. 알 수 없는 속성과 미래 버전은 거절합니다.

`variables` 선언은 화면 control의 존재와 순서를 결정합니다. `scope`는 Scope Selector, `range`는 Time Range Picker이며 선언하지 않은 kind의 control은 렌더하지 않습니다. 두 kind는 각각 단일 URL state에 연결되므로 같은 kind를 한 Dashboard에 두 번 선언하면 validation이 실패합니다. Refresh control은 공통 화면 동작으로 별도 유지합니다.

`queryRef`의 유일한 allowlist는 `apps/api/internal/querycatalog/defaults/*.yaml`입니다. 생성 단계는 모든 YAML을 정규 parser로 읽고 `panels[].series[].query`에서 `{queryRef, panelId, seriesKey}`만 sanitized mapping으로 생성합니다. 따라서 queryRef 이름 convention이나 별도 수동 목록에 의존하지 않으며, raw query text는 생성 결과나 browser bundle에 포함하지 않습니다.

## 데이터 계약과 migration

Dashboard는 ADR 0002의 `/scope`와 화면 단위 `/overview`만 사용합니다. `binding`은 기존 `ClusterOverviewResponse`의 고정 Section에만 연결됩니다. 표준 운영 Dashboard는 Overview가 실제 제공하는 정상 widget만 사용합니다. `TimeSeries`는 생성된 catalog mapping으로 기존 trend를 결합합니다. `LogStream`은 폐쇄형 registry 계약에는 포함되지만 Overview에 데이터가 없으므로, 사용한 별도 definition에서는 추가 요청 없이 widget-local unsupported 오류를 표시하는 안전 fallback입니다.

Migration은 명시적 step registry입니다. v1은 검증 후 identity이며 미래 버전을 추정 변환하지 않습니다. 편집, drag/drop, 저장, RBAC 편집은 #24 Post-MVP 범위입니다.
