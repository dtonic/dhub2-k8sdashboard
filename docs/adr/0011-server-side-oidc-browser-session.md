# ADR 0011: 서버 측 OIDC 브라우저 세션

- 상태: Proposed
- 날짜: 2026-08-15

## 결정

브라우저 로그인은 Authorization Code + PKCE를 BFF가 수행한다. 브라우저에는 `__Host-` HttpOnly 세션 ID와 CSRF 값만 전달하고 OIDC access/id/refresh token은 전달하지 않는다. 기존 Bearer 검증과 role→Scope 계약은 유지한다.

공유 Redis에는 SID/transaction ID의 SHA-256 키만 사용한다. 로그인 transaction과 refresh token은 `AUTH_SESSION_KEY`의 AES-256-GCM으로 암호화하며 record kind, Redis key, version, issuer, audience를 AAD로 결합한다. 요청당 세션 조회와 idle touch는 Lua 한 번으로 처리한다.

Redis key namespace는 issuer, 브라우저 client ID, API audience, public origin을 해시해 공유 Redis의 다른 배포와 transaction cap·session·refresh lock을 격리한다. 쿠키의 absolute Max-Age는 opaque handle의 보관 상한일 뿐 서버 세션의 유효성을 뜻하지 않는다. Redis idle/absolute 만료 뒤 stale 쿠키는 권한과 refresh를 모두 얻지 못하며 logout은 Redis record와 쿠키를 함께 제거한다.

PKCE public client와 confidential client를 모두 지원한다. `OIDC_CLIENT_SECRET`은 선택 사항이며 없으면 token form에 빈 값을 보내지 않는다. confidential client를 선택하면 Secret env로만 주입한다. 현재 구현은 provider 호환성을 위해 form secret을 사용하며 HTTP Basic 전환은 별도 provider 검증 후 결정한다.

API Bearer token은 기존 `OIDC_AUDIENCE`, 브라우저 ID token은 `OIDC_CLIENT_ID`를 검증한다. 만료된 role을 재사용하지 않고 최신 role을 얻기 위해 이 provider profile은 refresh 응답마다 role claim이 든 새 서명 ID token을 요구한다. OIDC Core는 refresh 응답의 ID token을 선택 사항으로 허용하므로 이를 제공하지 않는 provider는 현재 지원 범위 밖이다. refresh된 ID token이 없거나 유효하지 않으면 refresh-token rotation 가능성을 배제할 수 없어 세션을 폐기한다.

## 키 운용 계약

현재 형식은 단일 활성 키 `v1`이다. 키 변경은 이전 ciphertext를 fail-closed 처리하여 모든 기존 세션과 진행 중 로그인을 로그아웃시킨다. 정기 교체는 최대 absolute TTL 동안 기존 세션을 drain한 뒤 `secretRevision`과 함께 전체 replica를 조정 재시작한다. 키 유출 의심 시 즉시 교체하고 전 세션 무효화를 수용한다.

## 보안 경계

- HTTPS public origin, 정확한 callback, OIDC, 공유 Redis, 32-byte base64url Secret이 없으면 기동하지 않는다.
- 쿠키 인증 unsafe method는 설정된 origin과 세션별 CSRF를 정확히 비교하며 Forwarded 헤더를 신뢰하지 않는다.
- Redis 장애는 503, 없거나 만료·변조된 레코드는 401로 fail closed한다. 로그에는 SID, subject, token, plaintext/ciphertext를 남기지 않는다.
