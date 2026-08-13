# K8s Dashboard — 단일 진입점 명령
# 신규 개발자는 README §13만 보고 `make install && make dev`로 실행할 수 있어야 합니다.

.PHONY: install dev dev-web build lint test check design clean

install:            ## 의존성 설치
	npm install

dev: dev-web        ## 기본 개발 서버 (현재는 web만)

dev-web:            ## React UI (MSW mock API 위에서 단독 실행)
	npm run dev --workspace @k8s-dashboard/web

build:              ## 전체 빌드
	npm run build --workspaces --if-present

check:              ## 타입체크 + 디자인 시스템 preview 검증
	npm run check --workspaces --if-present

lint:               ## 린트 (도구 확정 후 연결)
	npm run lint --workspaces --if-present

test:               ## 테스트 (도구 확정 후 연결)
	npm run test --workspaces --if-present

design:             ## design-system preview 빌드 (Claude Design 업로드 대상)
	npm run build --workspace @k8s-dashboard/design-system

clean:
	rm -rf node_modules apps/web/dist design-system/dist
