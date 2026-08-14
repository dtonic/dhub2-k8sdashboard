# K8s Dashboard — 단일 진입점 명령
# 신규 개발자는 README §13만 보고 `make install && make dev`로 실행할 수 있어야 합니다.

.PHONY: install dev dev-web dev-api build build-web lint test test-web check design api-build api-test api-itest api-redis-itest api-vet deploy-check deploy-images clean

install:            ## 의존성 설치
	npm install

dev: dev-web        ## 기본 개발 서버 (현재는 web만)

dev-web:            ## React UI (MSW mock API 위에서 단독 실행)
	npm run dev --workspace @k8s-dashboard/web

dev-api:            ## Go Observability API/BFF (kubeconfig 또는 in-cluster)
	cd apps/api && go run ./cmd/api

build: build-web api-build  ## 전체 빌드 (Web + API)

build-web:          ## Web/디자인 시스템 빌드 (Go 툴체인 불필요 — CI Web job이 사용)
	npm run build --workspaces --if-present

check:              ## 타입체크 + 디자인 시스템 preview 검증
	npm run check --workspaces --if-present

lint: check api-vet ## 정적 검사 (TypeScript 타입체크 + go vet — 별도 린터 도입 전까지)

test: test-web api-test  ## 전체 테스트

test-web:           ## Web E2E (Playwright · mock API 위에서 실행)
	npm run test --workspace @k8s-dashboard/web

api-build:          ## Go API 빌드
	cd apps/api && go build ./...

api-test:           ## Go API 단위 테스트 (클러스터 불필요)
	cd apps/api && go test ./...

api-itest:          ## Go API 통합 테스트 (실제 kube-apiserver·GreptimeDB·Quickwit 대상)
	# Kubernetes — 둘 중 하나가 필요합니다.
	#   ITEST_KUBECONFIG=~/.kube/config   실제 클러스터. 기본은 읽기 전용입니다.
	#   KUBEBUILDER_ASSETS=<dir>          etcd/kube-apiserver 바이너리를 직접 띄웁니다.
	# 데이터소스 — 있으면 실인스턴스 검증이 함께 돕니다. 없으면 skip입니다.
	#   GREPTIME_ITEST_URL=http://localhost:4000    실제 GreptimeDB
	#   QUICKWIT_ITEST_URL=http://localhost:7280    실제 Quickwit
	#   QUICKWIT_ITEST_INDEX=k8s-logs               읽기 전용 커서 검증에 쓸 인덱스
	# 선택 —
	#   ITEST_MUTATE=1                    쓰기 검증. k8s는 임시 namespace/Pod,
	#                                     GreptimeDB는 k8s_dashboard_itest_metric 테이블,
	#                                     Quickwit은 k8s-dashboard-itest 인덱스만 만들고 지웁니다.
	#   ITEST_SERVICE_ACCOUNT=ns:name     배포된 ServiceAccount의 실제 권한 검사
	cd apps/api && go test -tags integration -count=1 -v -timeout 15m ./...

api-redis-itest:    ## Redis protocol/cache integration (REDIS_ITEST_ADDR required)
	cd apps/api && go test -tags integration -count=1 -v -timeout 2m ./internal/cache

api-vet:            ## Go 정적 검사
	cd apps/api && go vet ./...

deploy-check:       ## Helm lint/render, Kubernetes schema, repository policy
	sh deploy/scripts/check-deploy.sh

deploy-images:      ## Build release images locally without pushing
	docker build --file Dockerfile.web --tag observability-dashboard-web:ci .
	docker build --file Dockerfile.api --tag observability-dashboard-api:ci --build-arg VERSION=ci --build-arg COMMIT=$${GITHUB_SHA:-local} --build-arg BUILD_DATE=1970-01-01T00:00:00Z .

design:             ## design-system preview 빌드 (Claude Design 업로드 대상)
	npm run build --workspace @k8s-dashboard/design-system

clean:
	rm -rf node_modules apps/web/dist design-system/dist apps/web/playwright-report apps/web/test-results apps/api/bin
