# K8s Dashboard — 단일 진입점 명령
# 신규 개발자는 README §13만 보고 `make install && make dev`로 실행할 수 있어야 합니다.

.PHONY: install install-ci dev dev-web dev-api build build-web build-web-production lint test test-web test-web-integration web-unit contract-test web-performance dependency-audit check design api-build api-test api-coverage api-race api-performance api-govuln api-itest api-redis-itest api-vet api-proto-check api-central-itest quality-policy security-scan observability-check deploy-check deploy-images clean

install:            ## 의존성 설치
	npm install

install-ci:         ## Lockfile-exact clean install for CI
	npm install --global npm@12.0.2
	test "$$(npm --version)" = "12.0.2"
	( cd "$$(npm root --global)/npm" && npm pkg delete devDependencies && npm install --ignore-scripts --omit=dev --no-save brace-expansion@5.0.9 ip-address@10.3.1 )
	node scripts/quality/check-npm-toolchain.mjs
	npm ci --ignore-scripts

dev: dev-web        ## 기본 개발 서버 (현재는 web만)

dev-web:            ## React UI (MSW mock API 위에서 단독 실행)
	npm run dev --workspace @k8s-dashboard/web

dev-api:            ## Go Observability API/BFF (kubeconfig 또는 in-cluster)
	cd apps/api && go run ./cmd/api

build: build-web api-build  ## 전체 빌드 (Web + API)

build-web:          ## Web/디자인 시스템 빌드 (Go 툴체인 불필요 — CI Web job이 사용)
	npm run build --workspaces --if-present

build-web-production: ## Production Web bundle without MSW runtime
	npm run build --workspace @k8s-dashboard/web

check:              ## 타입체크 + 디자인 시스템 preview 검증
	npm run check --workspaces --if-present

lint: check api-vet ## 정적 검사 (TypeScript 타입체크 + go vet — 별도 린터 도입 전까지)

test: test-web api-test  ## 전체 테스트

test-web:           ## Web E2E (Playwright · mock API 위에서 실행)
	npm run test --workspace @k8s-dashboard/web

test-web-integration: ## 통합 E2E (#22 · 프로덕션 번들 + 실제 Go BFF fixture · 클러스터/Docker 불필요)
	cd apps/api && go test -tags e2efixture -count=1 ./internal/e2efixture/
	npm run build --workspace @k8s-dashboard/web
	npm run test:integration --workspace @k8s-dashboard/web

web-unit:           ## Vitest + Testing Library unit/component tests
	npm run test:unit --workspace @k8s-dashboard/web

contract-test:      ## OpenAPI/JSON schema and TypeScript parity tests
	npm test --workspace @k8s-dashboard/contracts
	npm test --workspace @k8s-dashboard/dashboard-schema

web-performance:   ## Deterministic production bundle raw/gzip budget
	python3 scripts/quality/check-performance.py --bundle-only

dependency-audit:  ## Exact reviewed npm advisory allowlist; high/critical always fail
	node scripts/quality/check-npm-audit.mjs
	node scripts/quality/check-npm-audit.mjs --self-test

api-build:          ## Go API 빌드
	cd apps/api && go build ./...

api-test:           ## Go API 단위 테스트 (클러스터 불필요)
	cd apps/api && go test ./...

api-coverage:       ## Merged >=86% plus documented package ratchets
	python3 scripts/quality/check-coverage.py

api-race:           ## Concurrency regression gate
	cd apps/api && go test -race -timeout 10m ./...

api-performance:    ## Allocation/byte budgets; ns/op is report-only
	python3 scripts/quality/check-performance.py --go-only
	python3 scripts/quality/check-performance.py --self-test

api-govuln:         ## Official fixed govulncheck module; zero reachable findings
	cd apps/api && go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...

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

api-postgres-itest: ## Dashboard metadata PostgreSQL CAS/migration integration
	cd apps/api && GOWORK=off go test -count=1 -v -timeout 2m ./internal/dashboard -run TestPostgresConcurrencyMigrationAndPrivacy

api-vet:            ## Go 정적 검사
	cd apps/api && go vet ./...

api-proto-check:    ## Versioned protocol generated-source drift
	sh deploy/scripts/check-cluster-state-proto.sh --self-test
	cd apps/api && go test -count=1 ./internal/clusterstate/protocol/v1

api-central-itest: ## Bounded private-CA registry/API integration
	cd apps/api && go test -count=1 -timeout 2m ./cmd/api -run 'TestCentralRuntimePrivateCA|TestServeCentralHTTP'
	cd apps/api && go test -count=1 -timeout 2m ./internal/clusterstate/transport

observability-check: ## Grafana, Prometheus rules and metric-reference drift
	sh deploy/scripts/check-observability.sh

deploy-check:       ## Helm lint/render, Kubernetes schema, repository policy
	sh deploy/scripts/check-deploy.sh

deploy-images:      ## Build release images locally without pushing
	docker build --target build --file Dockerfile.web --tag observability-dashboard-web-builder:ci .
	docker build --file Dockerfile.web --tag observability-dashboard-web:ci .
	docker build --file Dockerfile.api --tag observability-dashboard-api:ci --build-arg VERSION=ci --build-arg COMMIT=$${GITHUB_SHA:-local} --build-arg BUILD_DATE=1970-01-01T00:00:00Z .

quality-policy:     ## Required CI names and immutable supply-chain references
	python3 scripts/quality/check-workflow.py
	python3 scripts/quality/check-workflow.py --self-test

security-scan:      ## Secret, dependency, IaC and built-image HIGH/CRITICAL gates
	sh scripts/quality/security-scan.sh

design:             ## design-system preview 빌드 (Claude Design 업로드 대상)
	npm run build --workspace @k8s-dashboard/design-system

clean:
	rm -rf node_modules apps/web/dist design-system/dist apps/web/playwright-report apps/web/test-results apps/api/bin
