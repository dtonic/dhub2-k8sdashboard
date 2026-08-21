# ADR 0001 — 디자인 시스템을 `design-system/`에서 관리하고 Claude Design으로 동기화한다

- 상태: Superseded by ADR 0017 (#4)
- 날짜: 2026-08-13
- 결정자: @xenx96
- 관련: README §6 기술 스택, §7 저장소 구조, §12 로드맵 Phase 1
- 대체 결정: [ADR 0017 — dhub2-portal 디자인 시스템 원천 전환](https://github.com/dtonic/dhub2-k8sdashboard/issues/4)

## 배경

MVP 범위에는 Cluster Overview, Namespace Overview, Workload/Pod 상세, Logs Explorer,
Metric/Log/Event/Alert 상관분석이 모두 포함됩니다. 화면 수가 많고, 화면마다
"상태 배지 / KPI 타일 / 차트 프레임"이라는 같은 조각이 반복됩니다.

구현을 먼저 시작하면 다음 문제가 반복될 것이 예상됩니다.

1. **상태 색상의 난립.** CrashLoopBackOff, Pending, Replica 불일치 같은 상태는 화면마다
   다른 빨강/노랑으로 칠해지기 쉽습니다. 운영자가 색으로 심각도를 학습할 수 없게 됩니다.
2. **차트 색상의 비일관성과 접근성 결함.** 차트 라이브러리 기본 팔레트는 색각 이상 안전성을
   보장하지 않고, 계열 수에 따라 색을 순환 배정해 "같은 Pod가 화면마다 다른 색"이 됩니다.
3. **Dark 모드의 자동 반전.** 밝은 배경용 색을 그대로 뒤집으면 어두운 표면에서 대비가 무너집니다.
   관제 화면은 Dark 사용 비중이 높아 치명적입니다.
4. **리뷰 수단의 부재.** 디자인 논의가 스크린샷과 말로 오가면 무엇이 확정인지 남지 않습니다.

또한 이 프로젝트는 Grafana를 대체하지 않고 **운영 흐름에 특화된 UX**를 추가하는 것이 목적이므로,
UI 일관성 자체가 제품의 차별점입니다. 나중에 붙이는 것으로는 확보되지 않습니다.

## 결정

**구현 이전에 디자인 시스템을 먼저 세우고, 저장소 루트의 `design-system/` 디렉토리에서 관리한다.**

1. **원천은 git이다.** 디자인 토큰(`design-system/tokens/`)과 컴포넌트 스타일
   (`design-system/components/`)이 단일 원천이며, `apps/web`은 토큰만 참조한다.
   컴포넌트 코드에 원시 hex와 임의 픽셀값을 쓰지 않는다.
2. **리뷰 표면은 Claude Design이다.** 각 컴포넌트의 `*.preview.html`을 자기완결 HTML로 빌드해
   **design-sync**로 Claude Design의 Design System 프로젝트에 업로드한다. 팀은 거기서 카드 단위로
   보고 논의한다. 동기화는 **증분**으로 한 번에 한 컴포넌트씩 수행하며, 통째로 교체하지 않는다.
3. **차트 색은 검증된 팔레트만 쓴다.** `dataviz` 기준(명도 대역, 채도 하한, 인접 쌍 CVD 분리,
   정상 시야 하한, 표면 대비)을 Light/Dark 두 모드에서 통과한 8슬롯 고정 순서 팔레트를 채택한다.
   색을 바꿀 때는 validator를 재실행하고 결과를 `design-system/README.md`에 기록한다.
4. **상태 색은 예약한다.** `good / warning / serious / critical`은 차트 계열 색으로 재사용하지 않고,
   항상 아이콘 글리프 + 텍스트 라벨과 함께 렌더링한다.
5. **Light/Dark는 각각 선택한다.** 자동 반전하지 않고 각 표면에 맞춰 별도로 고른 값을
   `prefers-color-scheme`과 `:root[data-theme]` 양쪽에 선언한다. 사용자 토글이 OS 설정을 이긴다.

## 검토한 대안

| 대안 | 기각 사유 |
|---|---|
| 기성 UI 킷(MUI, Ant Design 등)을 그대로 사용 | 관제 화면에 필요한 정보 밀도와 상태 표현이 부족하고, 차트 팔레트의 색각 이상 안전성을 보장하지 않는다. 토큰 계층은 어차피 우리가 정의해야 한다. (기성 킷을 **컴포넌트 구현체**로 쓰는 것은 별개 결정으로 남겨둔다.) |
| Grafana 테마 변수를 그대로 차용 | Grafana를 대체하지 않는다는 원칙과 별개로, 우리 화면의 엔티티 모델(Workload/Pod 단위 드릴다운)에 맞는 상태 어휘가 없다. |
| Figma에서 관리 | 디자인 전담 인력이 없는 현재 구성에서 코드와 Figma의 이중 관리 비용이 크다. 코드가 원천이면 드리프트가 구조적으로 생기지 않는다. |
| 구현하면서 필요할 때 토큰을 추출 | 배경의 1~3번 문제가 그대로 발생한다. 화면이 늘어난 뒤의 소급 정리 비용이 선투자보다 크다. |
| Storybook 도입 | 나중에 도입할 수 있으나, 지금 필요한 것은 컴포넌트 개발 환경이 아니라 **합의 기록과 리뷰 표면**이다. 정적 preview + Claude Design으로 충분하며 빌드 의존성이 없다. |

## 결과

**좋아지는 것**

- 상태 색상과 차트 색상이 화면 간 일치한다. 운영자가 색으로 심각도를 학습할 수 있다.
- 접근성이 기본값으로 확보된다. 색각 이상 · 강제 색상 모드 · 저대비 표면이 사후 대응이 아니다.
- 디자인 합의가 git 히스토리와 Claude Design 카드에 남는다.
- Phase 3(Core UI)에서 화면 구현 속도가 붙는다. 매번 색과 간격을 다시 정하지 않는다.

**감수하는 것**

- Phase 1에 선투자가 필요하다. (토큰 + 7개 컴포넌트 + 빌드 스크립트)
- 색을 바꿀 때마다 validator 재실행이라는 절차가 생긴다. 의도한 마찰이다.
- 미리보기 빌드 산출물(`dist/`)과 원천의 동기화를 사람이 챙겨야 한다. CI의 `npm run check`로 보완한다.
- Claude Design 접근 권한이 없는 인원은 카드 리뷰에 참여할 수 없다. 이들에게는 저장소의
  preview 파일을 직접 열도록 안내한다.

## 후속 작업

- [x] 초기 토큰 + 핵심 컴포넌트(status-badge, stat-tile, chart-frame)
- [x] Pod Topology 및 부속 컴포넌트(data-table, time-range, log-modal)
- [ ] Claude Design에 Design System 타입 프로젝트 생성 후 최초 동기화
- [ ] CI에 `design-system` 빌드 검증(`npm run check`) 추가
- [ ] 컴포넌트 확장: Scope Selector, 로그 뷰어(Logs Explorer 본체), 빈/오류 상태, 아이콘 세트
- [ ] 아이콘 세트 결정 (인라인 SVG 스프라이트 여부) — 별도 ADR
- [ ] 차트 라이브러리 확정 (ECharts vs uPlot) 시 토큰 바인딩 레이어 정의 — 별도 ADR
