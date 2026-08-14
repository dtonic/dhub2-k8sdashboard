# K8s Dashboard — 단일 진입점 명령
# 신규 개발자는 README §13만 보고 `make install && make dev`로 실행할 수 있어야 합니다.

.PHONY: install dev dev-web dev-api build lint test test-web check design api-build api-test api-itest api-vet clean

install:            ## 의존성 설치
	npm install

dev: dev-web        ## 기본 개발 서버 (현재는 web만)

dev-web:            ## React UI (MSW mock API 위에서 단독 실행)
	npm run dev --workspace @k8s-dashboard/web

dev-api:            ## Go Observability API/BFF (kubeconfig 또는 in-cluster)
	cd apps/api && go run ./cmd/api

build:              ## 전체 빌드
	npm run build --workspaces --if-present

check:              ## 타입체크 + 디자인 시스템 preview 검증
	npm run check --workspaces --if-present

lint:               ## 린트 (도구 확정 후 연결)
	npm run lint --workspaces --if-present

test: test-web api-test  ## 전체 테스트

test-web:           ## Web E2E (Playwright · mock API 위에서 실행)
	npm run test --workspace @k8s-dashboard/web

api-build:          ## Go API 빌드
	cd apps/api && go build ./...

api-test:           ## Go API 단위 테스트 (클러스터 불필요)
	cd apps/api && go test ./...

api-itest:          ## Go API 통합 테스트 (실제 kube-apiserver 대상)
	# 대상 지정 — 둘 중 하나가 필요합니다.
	#   ITEST_KUBECONFIG=~/.kube/config   실제 클러스터. 기본은 읽기 전용입니다.
	#   KUBEBUILDER_ASSETS=<dir>          etcd/kube-apiserver 바이너리를 직접 띄웁니다.
	# 선택 —
	#   ITEST_MUTATE=1                    상태 반영 지연 측정 (객체를 만듭니다)
	#   ITEST_SERVICE_ACCOUNT=ns:name     배포된 ServiceAccount의 실제 권한 검사
	cd apps/api && go test -tags integration -count=1 -v -timeout 15m ./...

api-vet:            ## Go 정적 검사
	cd apps/api && go vet ./...

design:             ## design-system preview 빌드 (Claude Design 업로드 대상)
	npm run build --workspace @k8s-dashboard/design-system

clean:
	rm -rf node_modules apps/web/dist design-system/dist apps/web/playwright-report apps/web/test-results apps/api/bin
