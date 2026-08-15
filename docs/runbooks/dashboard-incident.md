# 운영자 장애 대응 · 롤백 Runbook (#22)

대시보드로 장애를 조사하는 표준 동선과, 대시보드 자체가 문제일 때의 대응·롤백
절차입니다. 플랫폼 자체 지표·알림은 [platform-observability.md](platform-observability.md)를,
릴리스 승인 기준은 [../release-checklist.md](../release-checklist.md)를 보세요.

## 배포 직후 점검

1. `/readyz`가 200인지 확인합니다 (503 = informer 미동기화 → 아래 참조).
2. `/version`이 배포한 태그·커밋과 일치하는지 확인합니다.
3. Cluster Overview를 열어 다섯 섹션(노드·Pod·이상 엔티티·추이·알림)이 모두
   값 또는 명시적 상태(권한 없음/강등)를 보여주는지 확인합니다. 전부 빈 화면이면
   Scope 설정 또는 인증 구성이 잘못된 것입니다.
4. 이상 엔티티 하나를 클릭해 Pod 상세 → "이 Pod의 로그 열기" 딥링크가 404 없이
   이어지는지 확인합니다.

## 장애 조사 동선 (4클릭 이내)

1. **Cluster Overview** — "이상 엔티티 Top N"에서 심각도·지속시간이 가장 나쁜 항목을
   봅니다. CrashLoopBackOff·ImagePullBackOff는 여기서 바로 드러납니다.
2. **원인 Pod 클릭** — Pod 상세에서 컨테이너 상태 · 재시작 수 · Warning Event ·
   CPU/메모리 추이를 한 화면에서 봅니다. CPU spike는 이 화면의 CPU 패널에서 보입니다.
3. **"이 Pod의 로그 열기"** — 같은 Pod UID·시간 범위로 Logs Explorer가 열립니다.
   ERROR 필터와 히스토그램으로 오류 구간을 좁힙니다.
4. **Alerts 교차 확인** — 알림 상세의 "관련 대상 상세 →"가 같은 Pod UID로
   돌아오면 같은 장애입니다.

URL이 `ns`·`uid`·시간 범위를 보존하므로, 조사 중인 화면 URL을 그대로 동료에게
공유하면 같은 것을 봅니다.

## 데이터소스 부분 장애 시

섹션의 "GreptimeDB 응답 없음" · "Quickwit 응답 없음" · "Alertmanager 응답 없음"은
대시보드 장애가 아니라 **해당 데이터소스 장애**입니다. Kubernetes 기반 화면
(목록·이벤트·상태)은 계속 동작하므로 조사를 이어갈 수 있습니다.

- 빈 화면 ≠ 권한 없음 ≠ 장애 — 세 상태는 다르게 표시됩니다. 어느 소스가 죽었는지는
  섹션의 출처 라벨로 구분합니다.
- 백엔드 `/metrics`의 upstream 오류·차단기 지표로 교차 확인합니다
  ([platform-observability.md §데이터소스 장애](platform-observability.md#데이터소스-장애)).
- Active가 정상인데 Resolved/counts만 `history_not_configured`이면 장애가 아닙니다.
  Alertmanager API v2는 현재 알림만 제공하며 운영 history adapter가 아직 없는 상태입니다.

## 대시보드 자체 장애 · 롤백

1. `/readyz` 503 → informer 미동기화:
   [platform-observability.md §Informer 미동기화](platform-observability.md#informer-미동기화).
2. 릴리스 직후 회귀라면 즉시 Helm 롤백:
   [../../deploy/README.md §롤백](../../deploy/README.md#롤백). 대시보드는 조회
   전용이라 데이터 마이그레이션이 없습니다 — 이전 이미지로 되돌리면 끝입니다.
   원인 조사는 롤백 뒤에 합니다.
3. 재배포 전에 [릴리스 체크리스트](../release-checklist.md)를 다시 통과시킵니다.

## 이 Runbook이 보장하지 않는 것

- 실제 GreptimeDB/Quickwit의 파괴 장애 재현은 자동 검증되지 않았습니다. Alertmanager
  Resolved 운영 이력은 Loki 또는 `GRAFANA_ALERTS` adapter가 생길 때까지 보장하지 않습니다.
- 알림 화면은 조회 전용입니다. Silence·Routing 조작은 Grafana/Alertmanager에서
  직접 수행합니다.
