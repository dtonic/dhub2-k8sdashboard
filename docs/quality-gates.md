# 품질 게이트와 Main 보호 정책

## 필수 CI

Main 보호에 등록하는 context는 아래 세 개뿐이며 이름을 바꾸거나 새 context를 추가하지 않습니다.

1. `Web (typecheck · build · e2e)` — typecheck, Vitest/Testing Library, 계약·schema parity,
   exact npm advisory gate, production bundle 예산, 전체 Playwright
2. `API (vet · test · build)` — Go 1.26.6, vet, merged/package coverage, race,
   govulncheck v1.1.4, Redis와 외부 adapter integration(skip 명시), build, allocation/byte 예산
3. `Deploy (images · Helm · schema · policy)` — 두 image rebuild, Helm/schema/policy,
   gitleaks 8.30.1, Trivy 0.74.0 dependency/IaC/image HIGH·CRITICAL gate와 독립 negative mutation

성능 기준은 `quality/budgets.json`이 단일 원천입니다. Go는 allocation과 bytes/op만 차단하고
ns/op은 보고만 합니다. Web은 production asset의 raw/gzip 합계를 차단합니다. 대시보드 요청 수(초기
전체 API 2건 이하, overview 1건)와 취소 전파는 기존 Playwright가 검증합니다.

React Router moderate 예외의 package, 설치 버전, advisory별 범위, fix 의미, 미사용 기능, 검토 기한은
`quality/npm-audit-allowlist.json`에 있습니다. 새 advisory, high/critical, 버전·범위·fix drift 또는 기한
초과는 실패합니다.

## 보호 적용 순서

아래 쓰기 명령은 이 변경을 원격에 올리고 해당 commit의 세 context가 모두 성공한 뒤에만 저장소 root가
실행합니다. 이 문서는 명령을 정의할 뿐 CI나 개발 작업에서 GitHub 설정을 변경하지 않습니다.

```bash
gh api --method PUT repos/xenx96/k8s-dashboard/branches/main/protection --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "Web (typecheck · build · e2e)",
      "API (vet · test · build)",
      "Deploy (images · Helm · schema · policy)"
    ]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": null,
  "restrictions": null,
  "required_conversation_resolution": true,
  "allow_force_pushes": false,
  "allow_deletions": false
}
JSON
```

정확한 readback:

```bash
gh api repos/xenx96/k8s-dashboard/branches/main/protection --jq '{
  strict: .required_status_checks.strict,
  contexts: .required_status_checks.contexts,
  enforce_admins: .enforce_admins.enabled,
  required_conversation_resolution: .required_conversation_resolution.enabled,
  allow_force_pushes: .allow_force_pushes.enabled,
  allow_deletions: .allow_deletions.enabled
}'
```

설정 자체를 되돌리는 정확한 rollback(보호 전체 삭제)은 다음과 같습니다.

```bash
gh api --method DELETE repos/xenx96/k8s-dashboard/branches/main/protection
```

`enforce_admins=false`는 남은 기능 작업 중 관리자가 긴급하게 direct-push할 수 있다는 trade-off가 있습니다.
품질 게이트가 안정화되고 남은 작업이 끝나면 `enforce_admins=true`로 올려 같은 우회를 막습니다.

Trivy 실행 이미지는 digest로 고정하지만, vulnerability DB는 CI 실행 시 최신 버전을 다운로드합니다. 따라서 코드 변경 없이도 새 CVE로 gate가 실패할 수 있으며, 이 경우 baseline·ignore를 추가하지 않고 취약 dependency 또는 base/toolchain digest를 fixed 버전으로 올린 뒤 전체 scan을 다시 실행합니다.
