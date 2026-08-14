# 릴리스 체크리스트 (#22)

MVP 릴리스 승인 기준입니다. 아래 게이트가 전부 통과해야 릴리스를 승인합니다.
개별 항목의 세부 정책은 [품질 게이트](quality-gates.md)를, 배포·롤백 절차는
[Helm 배포 §롤백](../deploy/README.md#롤백)을 따릅니다.

## 승인 게이트

- [ ] **필수 CI 3종 green** — `Web (typecheck · build · e2e)` · `API (vet · test · build)` ·
      `Deploy (images · Helm · schema · policy)` ([정책](quality-gates.md#필수-ci))
- [ ] **통합 E2E green** — `make test-web-integration`
      (프로덕션 번들 + 실제 Go BFF fixture · 클러스터/Docker 불필요)
  - 네 장애 시나리오(CrashLoopBackOff · ImagePullBackOff · CPU spike · Error log)가
    자동으로 재현되고 화면 증거(Metric·Log·Event·Alert 상관)가 확인됨
  - Overview → 원인 Pod → 관련 로그가 **4클릭 이내**, Pod UID·namespace·시간창 보존
  - 역할이 다른 두 사용자(브라우저 컨텍스트)의 데이터 격리와 범위 밖 딥링크 403
  - GreptimeDB · Quickwit · Alert backend 단독 중단 각각에서 Kubernetes 화면 유지 +
    해당 섹션만 출처 명시 degraded
- [ ] **배포 검증** — `make deploy-check observability-check` (render-only, apply 없음)
- [ ] **보안 게이트** — `make security-scan dependency-audit api-govuln`
- [ ] **운영 준비** — [장애 대응 Runbook](runbooks/dashboard-incident.md)과
      [플랫폼 관측성 Runbook](runbooks/platform-observability.md) 최신화 확인

## 알려진 제한 (릴리스 노트에 그대로 기재)

- **Alertmanager 실클라이언트는 미구현입니다(#17 잔여).** 알림 화면·상관분석은
  demo/fixture 소스로만 검증되었습니다. 실제 Alertmanager 대상 동작은 **미검증**입니다.
- **파괴적 실백엔드 시나리오는 미검증입니다.** 통합 E2E는 가짜 informer와 결정적
  데이터소스 위에서 돕니다. 실제 GreptimeDB/Quickwit의 장애 주입·복구, 실클러스터의
  Pod 재시작 유발 같은 파괴 검증은 수행하지 않았습니다 (`make api-itest`는 읽기 전용 기본).
- **UI는 아직 Authorization 헤더를 붙이지 않습니다.** `apps/web/src/api/client.ts`의
  fetch에는 인증 헤더가 없고, SSE(`/events/stream`) 제품 배선도 후속입니다. 통합 E2E는
  테스트 컨텍스트 헤더로 이 간극을 메워 검증했으며, 실사용자 로그인 흐름(토큰 획득·갱신·
  fetch/EventSource 헤더 전달)은 Web-API 실연결 후속 작업입니다.
- SSE 실시간 갱신은 백엔드 프로토콜만 구현되어 있고(ADR 0007) 제품 UI에는 연결되지
  않았습니다.

## 릴리스 절차 요약

1. `main`이 위 게이트를 전부 통과한 커밋인지 확인합니다.
2. [Helm 배포](../deploy/README.md) 절차로 배포합니다 (이미지 태그 고정).
3. 배포 직후 [장애 대응 Runbook](runbooks/dashboard-incident.md)의 "배포 직후 점검"을
   수행합니다.
4. 이상 시 [롤백](../deploy/README.md#롤백)을 즉시 실행합니다 — 원인 조사는 롤백 뒤에 합니다.
