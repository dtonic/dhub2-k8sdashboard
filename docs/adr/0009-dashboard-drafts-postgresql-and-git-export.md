# ADR 0009: Dashboard draft PostgreSQL 저장과 Git export

- 상태: Proposed
- 날짜: 2026-08-15

## 결정

Git의 Dashboard DSL v1 표준 파일은 읽기 전용 정본으로 유지한다. 사용자 편집본은 외부 PostgreSQL `dashboard_drafts` envelope에 OIDC `sub`, revision, 상태(`draft`/`submitted`/`approved`), schemaVersion, 검증된 definition JSONB, 시각을 저장한다. `dashboard.editor`는 자기 draft만 생성·수정·삭제·제출하며 `platform.admin`은 제출본 검토·승인만 한다.

모든 mutation은 `If-Match: "revision-N"`을 요구하고 SQL revision CAS로 갱신한다. 승인 snapshot은 변경·삭제할 수 없다. 다른 사용자의 draft는 제출 전 publisher에게도 보이지 않는다. 목록은 서명된 bounded keyset cursor를 사용한다.

서버는 runtime Query Catalog allowlist와 동일한 closed widget/layout 규칙으로 definition을 다시 검증한다. Raw query와 임의 component는 허용하지 않는다. 승인본만 typed definition을 deterministic 2-space JSON, trailing newline, SHA-256 ETag로 export한다. owner/revision/DB metadata는 제외하고 서버는 Git에 쓰지 않는다. 운영자가 export 파일을 검토·커밋한다.

Builder는 기본 비활성이다. 활성화 시 외부 PostgreSQL Secret, 32-byte 이상 cursor key, bounded pool/timeout, 명시적 egress CIDR/port가 필요하다. migration은 advisory lock으로 직렬화하며 미래 DB schema version은 시작 실패한다. DB가 준비되지 않으면 capability와 readiness가 fail closed 한다.

## 결과

- 표준 dashboard와 사용자 draft의 수명주기가 분리된다.
- 동시 편집은 조용히 덮어쓰지 않으며 UI는 로컬 변경을 유지한 채 reload/fork를 명시한다.
- 승인 dashboard는 Git 파일만으로 재현 가능하다.
- PostgreSQL은 이 chart가 설치하거나 소유하지 않는 외부 의존성이다.

운영 DB는 hostname 검증 TLS(`sslmode=verify-full`, 신뢰 CA/필요 시 `sslrootcert`)를 사용한다. migration rollback은 자동 downgrade가 아니라 이전 호환 API와 DB backup 복원 절차로 수행한다. Secret 값 회전은 `secretRevision` 증가로 rollout한다. NetworkPolicy port와 Secret DSN port가 일치해야 하며 불일치는 readiness 503에서 진단한다. 승인 export는 검토 후 파일을 표준 dashboard 경로에 커밋하고 기존 schema/CI loader 검증을 통과해야 한다.
