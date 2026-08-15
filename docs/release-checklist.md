# 릴리스 체크리스트 (#22)

- [ ] 브라우저 세션 사용 시 TLS ingress origin/callback, OIDC issuer egress, 공유 Redis, immutable Secret refs를 확인했다.
- [ ] `AUTH_SESSION_KEY` 정기 교체는 최대 absolute TTL drain 뒤 `secretRevision` 조정 재시작으로 수행하거나, 침해 시 즉시 전 세션 무효화를 승인했다.
- [ ] CSP `script-src`는 self-only이며 기존 inline style 때문에 `style-src 'unsafe-inline'` 예외만 존재함을 브라우저에서 확인했다.

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

- **Alertmanager 현재 알림 adapter는 opt-in입니다.** 운영 활성화 전 private CA·Bearer token
  Secret projection, 제한된 egress, matcher/scope 검증을 확인합니다. API v2는 해소 이력이
  아니므로 Resolved와 resolved 포함 counts는 `history_not_configured`입니다. Loki 또는
  `GRAFANA_ALERTS` history adapter는 P2 후속입니다(ADR 0012).
- **파괴적 실백엔드 시나리오는 미검증입니다.** 통합 E2E는 가짜 informer와 결정적
  데이터소스 위에서 돕니다. 실제 GreptimeDB/Quickwit의 장애 주입·복구, 실클러스터의
  Pod 재시작 유발 같은 파괴 검증은 수행하지 않았습니다 (`make api-itest`는 읽기 전용 기본).
- **브라우저 세션 provider profile은 refresh ID token을 요구합니다.** OIDC Core에서 이는
  선택 사항이므로, refresh 응답에 `OIDC_CLIENT_ID` audience와 최신 role claim이 든 서명
  ID token을 주지 않는 provider는 현재 호환되지 않습니다. 사전 provider 검증 없이
  `authSession.enabled`를 켜지 않습니다.
- 브라우저는 token 대신 same-origin HttpOnly 세션과 fetch-stream SSE를 사용합니다.
  릴리스 전 실제 nginx TLS 경로에서 refresh·logout·CSRF·Last-Event-ID replay와
  Redis idle/absolute 만료를 확인합니다.

## 릴리스 절차 요약

1. `main`이 위 게이트를 전부 통과한 커밋인지 확인합니다.
2. [Helm 배포](../deploy/README.md) 절차로 배포합니다 (이미지 태그 고정).
3. 배포 직후 [장애 대응 Runbook](runbooks/dashboard-incident.md)의 "배포 직후 점검"을
   수행합니다.
4. 이상 시 [롤백](../deploy/README.md#롤백)을 즉시 실행합니다 — 원인 조사는 롤백 뒤에 합니다.
