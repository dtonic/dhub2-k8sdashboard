# apps/web

React + TypeScript 기반 Custom Observability UI입니다. 현재 **Cluster Overview(이슈 #14)** 가 구현되어 있고,
나머지 화면은 라우트와 컨텍스트 전달만 잡힌 자리 표시자입니다.

API가 아직 없으므로 **MSW mock 위에서 단독 실행**됩니다. (이슈 #13 완료 기준)

---

## 실행

```bash
# 저장소 루트에서
make install
make dev            # http://localhost:5173

# 또는
npm run dev --workspace @k8s-dashboard/web
```

실제 API가 준비되면 `VITE_USE_MOCK=false`로 끕니다. 계약(`@k8s-dashboard/contracts`)은 그대로 씁니다.

## 상태 시나리오

Cluster Overview는 쿼리 파라미터로 상태를 재현할 수 있습니다. 리뷰와 스크린샷 검증에 씁니다.

| URL | 재현되는 상태 |
|---|---|
| `/` | 정상 |
| `/?scenario=degraded` | Quickwit·Alertmanager 장애 + GreptimeDB 부분 응답 (Network 패널 누락) |
| `/?scenario=forbidden` | Event·Alert·Topology 섹션이 권한 부족으로 거절 |
| `/?scenario=empty` | 모든 지표 정상, 이상 엔티티 0건 |
| `/?cluster=prod-frankfurt` | 화면 전체 403 (접근 불가 클러스터) |

Scope·시간 범위·자동 갱신 주기는 모두 URL 쿼리(`cluster`, `ns`, `range`, `refresh`)에 있습니다.
장애 대응 중 링크 하나로 같은 화면을 공유할 수 있어야 하기 때문입니다.

## 구조

```text
src/
├── api/            # 유일한 데이터 진입점 (client · TanStack Query 훅)
├── app/            # AppShell, 자리 표시자 라우트
├── components/     # 공통 조각 (상태, primitives, LineChart, 컨트롤)
├── features/
│   └── overview/   # Cluster Overview 화면과 패널
├── lib/            # 포맷터
├── mocks/          # MSW 핸들러와 고정 데이터
├── state/          # URL 기반 대시보드 파라미터
└── styles/         # design-system 참조 + 앱 셸 레이아웃
```

## 설계 규칙

### 요청

- **화면 하나 = 요청 하나.** Cluster Overview 전체가 `GET /api/v1/clusters/{id}/overview` 한 번입니다.
  위젯마다 훅을 만들면 초기 로딩에서 N+1이 발생합니다. (이슈 #14 완료 기준)
  실측: 첫 진입 시 API 요청 2건(`/scope` + `/overview`).
- 브라우저는 GreptimeDB·Quickwit·Kubernetes API를 **직접 호출하지 않습니다.** `src/api/client.ts`가 유일한 통로입니다.
- `signal`을 그대로 넘겨 요청 취소가 네트워크까지 전파되게 합니다.
- 프런트엔드는 Raw Query를 만들지 않습니다. 서버의 `queryRef`를 씁니다.

### 상태

- **"데이터 없음 · 권한 없음 · upstream 장애"는 서로 다른 화면입니다.** 세 경우를 같은 빈 화면으로 처리하면
  운영자가 원인을 오판합니다. `Section<T>` 봉투와 `SectionView`가 이를 강제합니다.
- 부분 장애는 패널 단위입니다. 한 데이터소스가 죽어도 나머지 패널은 값을 유지하고,
  장애 패널은 테두리 + stale 배지로 표시합니다.
- 권한 부족은 재시도하지 않습니다.

### 갱신

- 자동 갱신은 **데이터만** 바꿉니다. `placeholderData: keepPreviousData` 덕분에 범위를 바꿔도
  화면이 비워지지 않고 값만 교체됩니다. (DOM 노드 유지 확인 완료)
- Step은 사용자가 고르지 않습니다. 범위에 따라 서버가 강제하고 UI는 현재 Step을 표시만 합니다.

### 스타일

- 원시 hex·임의 픽셀값을 쓰지 않습니다. `design-system/tokens`의 역할 토큰만 참조합니다.
- 디자인 시스템 CSS는 복사하지 않고 `@import`로 그대로 참조합니다. 원천은 `design-system/`입니다. (ADR 0001)
- 차트는 라이브러리를 확정하기 전이라 얇은 자체 구현(`components/LineChart.tsx`)을 씁니다.
  ECharts/uPlot이 정해지면 이 파일만 교체합니다.

### 접근성

- 모든 폼 컨트롤에 라벨이 있고, 차트에는 `aria-label` 설명이 붙습니다.
- 상태는 색 단독으로 전달하지 않습니다. 배지의 글리프 + 텍스트가 항상 함께 갑니다.
- `main`/`nav` 랜드마크와 본문 건너뛰기 링크가 있습니다.
- `prefers-reduced-motion`을 존중합니다. 실시간 갱신 화면이라 특히 중요합니다.

## 남은 것

- 실제 API 연결 (`#8` Informer, `#9` Query Catalog, `#10` OIDC/RBAC 이후)
- Namespace·Workload·Pod 상세 (`#15`), Logs Explorer (`#16`), Alerts (`#17`)
- 표·차트 가상화 — 목록이 수천 건이 되면 필요합니다
- 린트·테스트 도구 확정 (`#21`)
