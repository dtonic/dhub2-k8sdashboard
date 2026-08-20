#!/usr/bin/env python3
import argparse
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "ci.yml"
MAKEFILE = ROOT / "Makefile"
SECURITY_SCAN = ROOT / "scripts" / "quality" / "security-scan.sh"
COVERAGE_CHECK = ROOT / "scripts" / "quality" / "check-coverage.py"
ROOT_PACKAGE = ROOT / "package.json"
WEB_DOCKERFILE = ROOT / "deploy" / "Dockerfile.web"
API_DOCKERFILE = ROOT / "deploy" / "Dockerfile.api"
GO_WORK = ROOT / "go.work"
API_GO_MOD = ROOT / "apps" / "api" / "go.mod"
README = ROOT / "README.md"
NPM_TOOLCHAIN_CHECK = ROOT / "scripts" / "quality" / "check-npm-toolchain.mjs"
COLLECTOR_DOCKERFILE = ROOT / "deploy" / "telemetry" / "Dockerfile.collector"
COLLECTOR_BUILDER = ROOT / "deploy" / "telemetry" / "collector-builder.yaml"
MOCK_DOCKERFILE = ROOT / "deploy" / "telemetry" / "Dockerfile.mock"
COLLECTOR_BUILD_CHECK = ROOT / "deploy" / "scripts" / "build-telemetry-collector.sh"
TELEMETRY_IMAGE_CHECK = ROOT / "deploy" / "scripts" / "check-telemetry-images.sh"
ALERTMANAGER_CHECK = ROOT / "deploy" / "scripts" / "check-alertmanager-integration.sh"
ALERTMANAGER_IMAGE = "quay.io/prometheus/alertmanager@sha256:27c475db5fb156cab31d5c18a4251ac7ed567746a2483ff264516437a39b15ba"
ALERT_HOST_PROXY = 'python3 "$ROOT/deploy/alertmanager/proxy.py" --upstream "$ADMIN_URL"'
ALERT_BROWSER_PROXY_MOUNT = '-v "$ROOT/deploy/alertmanager/proxy.py:/proxy.py:ro"'
ALERT_BROWSER_PROXY_RUN = 'python3 /proxy.py --container-network'
ALERT_BROWSER_FIXED_BIND = '-backend-addr "0.0.0.0:9444"'
ALERT_BROWSER_MAIN_DNS = 's/__UPSTREAM_HOST__/auth-main/g'
ALERT_BROWSER_OUTAGE_DNS = 's/__UPSTREAM_HOST__/auth-outage/g'
ACTION_PINS = {
    "actions/checkout": "11d5960a326750d5838078e36cf38b85af677262",
    "actions/setup-node": "49933ea5288caeca8642d1e84afbd3f7d6820020",
    "actions/setup-go": "40f1582b2485089dde7abd97c1529aa768e1baff",
    "actions/upload-artifact": "ea165f8d65b6e75b540449e92b4886f43607fa02",
}
REDIS = "redis:8.2.6-alpine@sha256:ea5a07305d6c66f99df5a5ff8d9659e8f6cb598e6e586dc8dd92b7fcd915746e"
POSTGRES = "cgr.dev/chainguard/postgres@sha256:844baac51caa0212727f9a53f25beec94cedb6778c06c75e3f7bb092079142f3"
POSTGRES_TEST_URL_LINE = "DASHBOARD_POSTGRES_TEST_URL: postgres://postgres:dashboard-ci-only@127.0.0.1:5432/dashboard_ci?sslmode=disable"
GO_VERSION = "1.26.6"
GO_LANGUAGE_VERSION = "1.26.0"
GO_BUILDER = "golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df"
NODE_VERSION = "22.23.2"
NODE_BUILDER = "node:22.23.2-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32"
NPM_VERSION = "12.0.2"
NPM_FIXED_DEPS = "brace-expansion@5.0.9 ip-address@10.3.1"
WEB_BUILDER_TAG = "observability-dashboard-web-builder:ci"
GITLEAKS = "ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f"
TRIVY = "aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969"
HELM = "alpine/helm:3.17.3@sha256:d899e6316789fec04ee95300a18e454b7942539cbb3d89bde3e0655d6ca2e895"
TELEMETRY_SUPPLY_PINS = (
    "golang:1.26.6-alpine@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df",
    "gcr.io/distroless/static-debian13@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6",
    "go.opentelemetry.io/collector/cmd/builder@v0.158.0",
    "alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
    "https://dl-cdn.alpinelinux.org/alpine/v3.24/main",
    "python3=3.14.7-r1",
    TRIVY,
    "bff6e429b67b94bc95659387ac4240fa19c3ca3f49e7a8afcd2a1dbc35ccd442",
    "42d03618eaf737b778612108b0352a506ea3625830189dd5a77f8f44c7dcf503",
    "c544e7cf18f0f44c82917877dd664dda943f52a36a1dda1d948f94a5244f5030",
)
GOVULN = "golang.org/x/vuln/cmd/govulncheck@v1.1.4"
GENERATED_PROTO_PACKAGE = "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
REQUIRED_JOBS = {
    "Web (typecheck · build · e2e)",
    "API (vet · test · build)",
    "Deploy (images · Helm · schema · policy)",
}
REQUIRED_JOB_IDS = {"web", "api", "deploy"}

def validate(text, makefile, security_scan, package_source=None, docker_source=None, npm_tool_source=None,
             go_work_source=None, go_mod_source=None, api_docker_source=None, readme_source=None,
             telemetry_supply_source=None, coverage_source=None, alertmanager_source=None):
    errors = []
    if coverage_source is None:
        coverage_source = COVERAGE_CHECK.read_text(encoding="utf-8")
    declaration = f'GENERATED_PROTO_PACKAGE = "{GENERATED_PROTO_PACKAGE}"'
    exact_filter = "if package == GENERATED_PROTO_PACKAGE"
    if coverage_source.count(declaration) != 1 or coverage_source.count(exact_filter) != 1:
        errors.append("generated protobuf coverage exclusion must be one exact package equality")
    if not re.search(r"(?m)^permissions:\s*\n\s+contents:\s*read\s*$", text):
        errors.append("top-level contents: read permission is missing")
    job_names = re.findall(r"(?m)^ {4}name:\s+(.+?)\s*$", text)
    job_ids = re.findall(r"(?m)^ {2}([A-Za-z0-9_-]+):\s*$", text.split("\njobs:\n", 1)[-1])
    if (len(job_names) != len(REQUIRED_JOBS) or set(job_names) != REQUIRED_JOBS
            or len(job_ids) != len(REQUIRED_JOB_IDS) or set(job_ids) != REQUIRED_JOB_IDS):
        errors.append("required job names drifted")
    for action, expected in ACTION_PINS.items():
        refs = re.findall(rf"uses:\s*{re.escape(action)}@([^\s]+)", text)
        if not refs or any(ref != expected for ref in refs):
            errors.append(f"mutable or unexpected action pin: {action}")
    for action, ref in re.findall(r"uses:\s*([^@\s]+)@([^\s]+)", text):
        if not re.fullmatch(r"[0-9a-f]{40}", ref):
            errors.append(f"action is not pinned to a full SHA: {action}@{ref}")
    service_images = re.findall(r"(?m)^\s{8}image:\s*(\S+)", text)
    if service_images != [REDIS, POSTGRES]:
        errors.append("service images are not pinned to the approved manifest digests")
    if text.count("run: make api-postgres-itest") != 1:
        errors.append("dashboard PostgreSQL integration step is missing or duplicated")
    if text.count("run: make api-alertmanager-itest") != 1:
        errors.append("Alertmanager integration step is missing or duplicated")
    web_match = re.search(r"(?ms)^  web:\s*$.*?(?=^  [A-Za-z0-9_-]+:\s*$|\Z)", text)
    web_job = web_match.group(0) if web_match else ""
    ordered_web_markers = (
        "run: make build-web-production",
        "run: npx playwright install --with-deps chromium",
        "uses: actions/setup-go@",
        "run: make api-alertmanager-itest",
    )
    positions = [web_job.find(marker) for marker in ordered_web_markers]
    if any(position < 0 for position in positions) or positions != sorted(positions) or web_job.count("run: make api-alertmanager-itest") != 1:
        errors.append("Alertmanager integration must run once in Web after production build, Chromium, and Go setup")
    alertmanager_target = re.search(r"(?ms)^api-alertmanager-itest:.*?(?=^\S|\Z)", makefile)
    alertmanager_target_text = alertmanager_target.group(0) if alertmanager_target else ""
    if alertmanager_target_text.count("sh deploy/scripts/check-alertmanager-integration.sh") != 1:
        errors.append("Alertmanager integration target is missing or duplicated")
    if alertmanager_source is None:
        alertmanager_source = ALERTMANAGER_CHECK.read_text(encoding="utf-8")
    if alertmanager_source.count(ALERTMANAGER_IMAGE) != 1:
        errors.append("Alertmanager integration image digest drifted")
    for required in (ALERT_HOST_PROXY, ALERT_BROWSER_PROXY_MOUNT, ALERT_BROWSER_PROXY_RUN,
                     "go test -tags integration", "TestActualAlertmanagerPrivateCABearerScopeAndFailures"):
        if alertmanager_source.count(required) != 1:
            errors.append("Alertmanager production integration command drifted")
    if alertmanager_source.count(ALERT_BROWSER_FIXED_BIND) != 2:
        errors.append("Alertmanager browser backends must use the fixed isolated container port")
    for marker in (ALERT_BROWSER_MAIN_DNS, ALERT_BROWSER_OUTAGE_DNS):
        if alertmanager_source.count(marker) != 1:
            errors.append("Alertmanager browser nginx must use owned-network DNS upstreams")
    auth_runs = re.findall(
        r'(?ms)^AUTH_(?:MAIN|OUTAGE)_ID=\$\(docker run .*?^\s+-alertmanager-client-key-file .*?\)$',
        alertmanager_source,
    )
    if len(auth_runs) != 2:
        errors.append("Alertmanager browser auth container commands drifted")
    forbidden_auth_cli = re.compile(
        r'(?<!\S)(?:--ip(?:=|\s)|--network(?:=host|\s+host)(?=\s|\\|$)|-p(?:=|\s)|--publish(?:=|\s))'
    )
    if any(forbidden_auth_cli.search(command) for command in auth_runs):
        errors.append("Alertmanager browser auth containers must not use static IP, host network, or published ports")
    if "SUBNET=" in alertmanager_source:
        errors.append("Alertmanager browser fixture must not depend on static IPAM or host networking")
    if text.count(POSTGRES_TEST_URL_LINE) != 2:
        errors.append("dashboard PostgreSQL integration URL drifted")
    coverage_step = "      - run: make api-coverage\n        env:\n          " + POSTGRES_TEST_URL_LINE
    if text.count(coverage_step) != 1:
        errors.append("coverage step must run the actual PostgreSQL suite with the fixed CI URL")
    # Web job(통합 E2E fixture)과 API job이 같은 고정 패치를 정확히 한 번씩 씁니다.
    go_versions = re.findall(r'(?m)^\s+go-version:\s*["\']?([^"\'\s]+)', text)
    if go_versions != [GO_VERSION, GO_VERSION]:
        errors.append("setup-go version is not fixed to the approved patch")
    if text.count("run: make test-web-integration") != 1:
        errors.append("integration E2E workflow step is missing or duplicated")
    integration_target = re.search(r"(?ms)^test-web-integration:.*?(?=^\S|\Z)", makefile)
    integration_text = integration_target.group(0) if integration_target else ""
    for required in (
        "cd apps/api && go test -tags e2efixture -count=1 ./internal/e2efixture/",
        "npm run build --workspace @k8s-dashboard/web",
        "npm run test:integration --workspace @k8s-dashboard/web",
    ):
        if required not in integration_text:
            errors.append("integration E2E target drifted")
    if "VITE_USE_MOCK=" in integration_text or "--mode e2e" in integration_text:
        errors.append("integration E2E must build the default production bundle")
    go_work_text = GO_WORK.read_text(encoding="utf-8") if go_work_source is None else go_work_source
    go_mod_text = API_GO_MOD.read_text(encoding="utf-8") if go_mod_source is None else go_mod_source
    api_docker_text = API_DOCKERFILE.read_text(encoding="utf-8") if api_docker_source is None else api_docker_source
    readme_text = README.read_text(encoding="utf-8") if readme_source is None else readme_source
    for label, module_text in (("go.work", go_work_text), ("apps/api/go.mod", go_mod_text)):
        if not re.search(rf"(?m)^go {re.escape(GO_LANGUAGE_VERSION)}$", module_text):
            errors.append(f"{label} language version drifted")
        if not re.search(rf"(?m)^toolchain go{re.escape(GO_VERSION)}$", module_text):
            errors.append(f"{label} toolchain patch drifted")
    if f"FROM {GO_BUILDER} AS build" not in api_docker_text:
        errors.append("API Go builder is not pinned to the approved manifest digest")
    if f"- Go >= {GO_VERSION}" not in readme_text:
        errors.append("README Go minimum drifted")
    node_versions = re.findall(r'(?m)^\s+node-version:\s*["\']?([^"\'\s]+)', text)
    if node_versions != [NODE_VERSION]:
        errors.append("setup-node version is not fixed to the maintained patch")
    package_text = ROOT_PACKAGE.read_text(encoding="utf-8") if package_source is None else package_source
    docker_text = WEB_DOCKERFILE.read_text(encoding="utf-8") if docker_source is None else docker_source
    if f'"node": ">={NODE_VERSION}"' not in package_text:
        errors.append("root Node engine minimum drifted")
    if f"FROM {NODE_BUILDER} AS build" not in docker_text:
        errors.append("Web Node builder is not pinned to the approved manifest digest")
    if f"npm install --global npm@{NPM_VERSION}" not in makefile or f'npm install --global npm@{NPM_VERSION}' not in docker_text:
        errors.append("npm is not installed at the approved exact version in CI and Docker")
    if makefile.count(f'test "$$(npm --version)" = "{NPM_VERSION}"') != 1 or f'test "$(npm --version)" = "{NPM_VERSION}"' not in docker_text:
        errors.append("npm version assertion drifted")
    install_fixed = f"npm install --ignore-scripts --omit=dev --no-save {NPM_FIXED_DEPS}"
    if makefile.count(install_fixed) != 1 or install_fixed not in docker_text:
        errors.append("npm vulnerable transitive dependency remediation drifted")
    if makefile.count("node scripts/quality/check-npm-toolchain.mjs") != 1 or "node /tmp/check-npm-toolchain.mjs" not in docker_text:
        errors.append("npm filesystem/tool resolver assertion is not run in CI and Docker")
    npm_tool_text = NPM_TOOLCHAIN_CHECK.read_text(encoding="utf-8") if npm_tool_source is None else npm_tool_source
    for expected in ('const EXPECTED_NPM = "12.0.2"', '["brace-expansion", "5.0.9"]', '["ip-address", "10.3.1"]'):
        if expected not in npm_tool_text:
            errors.append("npm toolchain exact-version assertion drifted")
    if makefile.count(WEB_BUILDER_TAG) != 1 or WEB_BUILDER_TAG not in security_scan:
        errors.append("Web builder image is not built and scanned under the approved tag")
    if makefile.count("npm ci --ignore-scripts") != 1 or "npm ci --ignore-scripts" not in docker_text:
        errors.append("CI and Docker npm clean-install lifecycle policy drifted")
    if "make api-govuln" not in text or GOVULN not in makefile:
        errors.append("govulncheck is not pinned to v1.1.4 in the API job")
    if "make security-scan" not in text or GITLEAKS not in security_scan:
        errors.append("gitleaks image is not pinned to the approved digest")
    if "make security-scan" not in text or TRIVY not in security_scan:
        errors.append("Trivy image is not pinned to the approved digest")
    if security_scan.count(HELM) != 1:
        errors.append("security Helm image is not pinned to the approved digest")
    if security_scan.count(POSTGRES) != 1:
        errors.append("PostgreSQL integration image is not scanned at its approved digest")
    for required in (
        'docker pull "$POSTGRES_IMAGE"',
        'docker save --output "$TMP/postgres-image.tar" "$POSTGRES_IMAGE"',
        '--input /scan/postgres-image.tar',
    ):
        if security_scan.count(required) != 1:
            errors.append("PostgreSQL scan must pull its pinned digest and scan a local tar")
    if '--no-progress "$POSTGRES_IMAGE"' in security_scan:
        errors.append("PostgreSQL scan must not fetch layers remotely through Trivy")
    if "negative-privileged.yaml" not in security_scan or "KSV-0017" not in security_scan:
        errors.append("Trivy privileged-workload negative fixture is missing")
    if telemetry_supply_source is None:
        telemetry_supply_source = "\n".join(path.read_text(encoding="utf-8") for path in (
            COLLECTOR_DOCKERFILE, COLLECTOR_BUILDER, MOCK_DOCKERFILE, COLLECTOR_BUILD_CHECK, TELEMETRY_IMAGE_CHECK,
        ))
    for pin in TELEMETRY_SUPPLY_PINS:
        if telemetry_supply_source.count(pin) == 0:
            errors.append(f"telemetry supply-chain pin drifted: {pin}")
    component_versions = re.findall(r"(?m)^\s*- gomod: \S+ (v\S+)$", telemetry_supply_source)
    if len(component_versions) != 15 or set(component_versions) != {"v0.158.0"}:
        errors.append("telemetry OCB component versions drifted")
    if telemetry_supply_source.count("builder --skip-compilation --config /src/collector-builder.yaml") != 1:
        errors.append("telemetry OCB generation must skip its implicit compilation")
    if "skip_compilation:" in telemetry_supply_source:
        errors.append("unsupported telemetry OCB manifest skip_compilation key is present")
    return errors

parser = argparse.ArgumentParser()
parser.add_argument("--self-test", action="store_true")
args = parser.parse_args()
source = WORKFLOW.read_text(encoding="utf-8")
makefile_source = MAKEFILE.read_text(encoding="utf-8")
security_source = SECURITY_SCAN.read_text(encoding="utf-8")
if args.self_test:
    mutations = [
        ("mutable action", source.replace(f"actions/checkout@{ACTION_PINS['actions/checkout']}", "actions/checkout@v4", 1), makefile_source, security_source),
        ("mutable service", source.replace(REDIS, "redis:8.2.6-alpine", 1), makefile_source, security_source),
        ("mutable postgres service", source.replace(POSTGRES, "cgr.dev/chainguard/postgres:latest", 1), makefile_source, security_source),
        ("postgres scan removed", source, makefile_source, security_source.replace(POSTGRES, "cgr.dev/chainguard/postgres:latest", 1)),
        ("postgres pinned pull removed", source, makefile_source, security_source.replace('docker pull "$POSTGRES_IMAGE"', "true", 1)),
        ("postgres local tar input removed", source, makefile_source, security_source.replace("--input /scan/postgres-image.tar", "--input /scan/missing.tar", 1)),
        ("Trivy negative fixture removed", source, makefile_source, security_source.replace("KSV-0017", "KSV-0000")),
        ("postgres test removed", source.replace("run: make api-postgres-itest", "run: true", 1), makefile_source, security_source),
        ("coverage postgres env removed", source.replace("      - run: make api-coverage\n        env:\n          " + POSTGRES_TEST_URL_LINE, "      - run: make api-coverage", 1), makefile_source, security_source),
        ("coverage postgres env drift", source.replace(POSTGRES_TEST_URL_LINE, POSTGRES_TEST_URL_LINE.replace("sslmode=disable", "sslmode=require"), 1), makefile_source, security_source),
        ("postgres test env drift", source.replace("      - run: make api-postgres-itest\n        env:\n          " + POSTGRES_TEST_URL_LINE, "      - run: make api-postgres-itest\n        env:\n          " + POSTGRES_TEST_URL_LINE.replace("sslmode=disable", "sslmode=require"), 1), makefile_source, security_source),
        ("mutable govulncheck", source, makefile_source.replace(GOVULN, "golang.org/x/vuln/cmd/govulncheck@latest", 1), security_source),
        ("mutable gitleaks", source, makefile_source, security_source.replace(GITLEAKS, "ghcr.io/gitleaks/gitleaks:v8.30.1", 1)),
        ("mutable Trivy", source, makefile_source, security_source.replace(TRIVY, "aquasec/trivy:0.74.0", 1)),
        ("mutable security Helm", source, makefile_source, security_source.replace(HELM, "alpine/helm:3.17.3", 1)),
        ("job rename masked by step", source.replace(next(iter(REQUIRED_JOBS)), "Renamed job", 1) + f"\n      - name: {next(iter(REQUIRED_JOBS))}\n        run: true\n", makefile_source, security_source),
        ("extra job", source + "\n  unexpected:\n    name: Unexpected quality job\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("unnamed extra job", source + "\n  unnamed-extra:\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("duplicate web override", source + "\n  web:\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("Go version drift", source.replace(f'go-version: "{GO_VERSION}"', 'go-version: "1.26.7"', 1), makefile_source, security_source),
        ("integration step removed", source.replace("run: make test-web-integration", "run: true", 1), makefile_source, security_source),
        ("Alertmanager integration step removed", source.replace("run: make api-alertmanager-itest", "run: true", 1), makefile_source, security_source),
        ("Alertmanager integration moved out of Web", source.replace("run: make api-alertmanager-itest", "run: true", 1).replace("run: make api-vet", "run: make api-alertmanager-itest", 1), makefile_source, security_source),
        ("Alertmanager integration ordered before production prerequisites", source.replace("run: make build-web-production", "__ALERT_ORDER_SWAP__", 1).replace("run: make api-alertmanager-itest", "run: make build-web-production", 1).replace("__ALERT_ORDER_SWAP__", "run: make api-alertmanager-itest", 1), makefile_source, security_source),
        ("Alertmanager integration target removed", source, makefile_source.replace("sh deploy/scripts/check-alertmanager-integration.sh", "true", 1), security_source),
        ("integration mock env mutation", source, makefile_source.replace("\tnpm run build --workspace @k8s-dashboard/web\n\tnpm run test:integration", "\tVITE_USE_MOCK=true npm run build --workspace @k8s-dashboard/web\n\tnpm run test:integration", 1), security_source),
        ("integration mock mode mutation", source, makefile_source.replace("\tnpm run build --workspace @k8s-dashboard/web\n\tnpm run test:integration", "\tnpm run build --workspace @k8s-dashboard/web -- --mode e2e\n\tnpm run test:integration", 1), security_source),
        ("Node version drift", source.replace(f"node-version: {NODE_VERSION}", "node-version: 22.23.1", 1), makefile_source, security_source),
    ]
    for label, mutated_workflow, mutated_makefile, mutated_security in mutations:
        if not validate(mutated_workflow, mutated_makefile, mutated_security):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    alertmanager_source = ALERTMANAGER_CHECK.read_text(encoding="utf-8")
    for label, mutated_alertmanager in (
        ("Alertmanager image digest drift", alertmanager_source.replace(ALERTMANAGER_IMAGE, "quay.io/prometheus/alertmanager:latest", 1)),
        ("Alertmanager production test drift", alertmanager_source.replace("TestActualAlertmanagerPrivateCABearerScopeAndFailures", "TestMissing", 1)),
        ("Alertmanager host proxy removed", alertmanager_source.replace(ALERT_HOST_PROXY, "true # host proxy removed", 1)),
        ("Alertmanager browser proxy mount drift", alertmanager_source.replace(ALERT_BROWSER_PROXY_MOUNT, '-v "$ROOT/deploy/alertmanager/proxy.py:/proxy.py"', 1)),
        ("Alertmanager browser proxy run removed", alertmanager_source.replace(ALERT_BROWSER_PROXY_RUN, "python3 /proxy.py", 1)),
        ("Alertmanager browser proxy duplicated", alertmanager_source + "\n# " + ALERT_BROWSER_PROXY_RUN + "\n"),
        ("Alertmanager browser fixed bind drift", alertmanager_source.replace(ALERT_BROWSER_FIXED_BIND, '-backend-addr "0.0.0.0:0"', 1)),
        ("Alertmanager browser main DNS drift", alertmanager_source.replace(ALERT_BROWSER_MAIN_DNS, 's/__UPSTREAM_HOST__/127.0.0.1/g', 1)),
        ("Alertmanager browser outage DNS drift", alertmanager_source.replace(ALERT_BROWSER_OUTAGE_DNS, 's/__UPSTREAM_HOST__/127.0.0.1/g', 1)),
        ("Alertmanager browser static IP dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --ip 10.0.0.10 --user", 1)),
        ("Alertmanager browser static IP equals dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --ip=10.0.0.10 --user", 1)),
        ("Alertmanager browser host network dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --network host --user", 1)),
        ("Alertmanager browser host network equals dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --network=host --user", 1)),
        ("Alertmanager browser short publish dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main -p 9444:9444 --user", 1)),
        ("Alertmanager browser publish dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --publish 9444:9444 --user", 1)),
        ("Alertmanager browser publish equals dependency", alertmanager_source.replace("--network-alias auth-main --user", "--network-alias auth-main --publish=9444:9444 --user", 1)),
    ):
        if not validate(source, makefile_source, security_source, alertmanager_source=mutated_alertmanager):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    coverage_source = COVERAGE_CHECK.read_text(encoding="utf-8")
    for label, mutated_coverage in (
        ("generated coverage path drift", coverage_source.replace(GENERATED_PROTO_PACKAGE, GENERATED_PROTO_PACKAGE + "-drift", 1)),
        ("generated coverage broadening", coverage_source.replace("if package == GENERATED_PROTO_PACKAGE", "if package.endswith('/protocol/v1')", 1)),
        ("generated coverage removal", coverage_source.replace("if package == GENERATED_PROTO_PACKAGE", "if False", 1)),
    ):
        if not validate(source, makefile_source, security_source, coverage_source=mutated_coverage):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    package_source = ROOT_PACKAGE.read_text(encoding="utf-8")
    docker_source = WEB_DOCKERFILE.read_text(encoding="utf-8")
    node_files = [
        ("Node engine drift", package_source.replace(f'">={NODE_VERSION}"', '">=22.23.1"', 1), docker_source),
        ("Node builder drift", package_source, docker_source.replace(NODE_BUILDER, "node:22.23.2-alpine", 1)),
        ("Docker npm drift", package_source, docker_source.replace(f"npm@{NPM_VERSION}", "npm@latest", 1)),
    ]
    for label, mutated_package, mutated_docker in node_files:
        if not validate(source, makefile_source, security_source, mutated_package, mutated_docker):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    go_work_source = GO_WORK.read_text(encoding="utf-8")
    go_mod_source = API_GO_MOD.read_text(encoding="utf-8")
    api_docker_source = API_DOCKERFILE.read_text(encoding="utf-8")
    readme_source = README.read_text(encoding="utf-8")
    go_files = [
        ("go.work language drift", go_work_source.replace(f"go {GO_LANGUAGE_VERSION}", "go 1.25.0", 1), go_mod_source, api_docker_source, readme_source),
        ("go.work toolchain drift", go_work_source.replace(f"toolchain go{GO_VERSION}", "toolchain go1.26.5", 1), go_mod_source, api_docker_source, readme_source),
        ("go.mod language drift", go_work_source, go_mod_source.replace(f"go {GO_LANGUAGE_VERSION}", "go 1.25.0", 1), api_docker_source, readme_source),
        ("go.mod toolchain drift", go_work_source, go_mod_source.replace(f"toolchain go{GO_VERSION}", "toolchain go1.26.5", 1), api_docker_source, readme_source),
        ("API Go builder drift", go_work_source, go_mod_source, api_docker_source.replace(GO_BUILDER, "golang:1.26.6-alpine", 1), readme_source),
        ("README Go minimum drift", go_work_source, go_mod_source, api_docker_source, readme_source.replace(f"Go >= {GO_VERSION}", "Go >= 1.24", 1)),
    ]
    for label, mutated_work, mutated_mod, mutated_api_docker, mutated_readme in go_files:
        if not validate(source, makefile_source, security_source, go_work_source=mutated_work,
                        go_mod_source=mutated_mod, api_docker_source=mutated_api_docker,
                        readme_source=mutated_readme):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    npm_make_drift = makefile_source.replace(f"npm@{NPM_VERSION}", "npm@latest", 1)
    if not validate(source, npm_make_drift, security_source):
        raise SystemExit("CI npm drift mutation was masked")
    print("negative mutation passed: CI npm drift was rejected")
    lifecycle_drift = makefile_source.replace("npm ci --ignore-scripts", "npm ci", 1)
    if not validate(source, lifecycle_drift, security_source):
        raise SystemExit("CI npm lifecycle policy mutation was masked")
    print("negative mutation passed: CI npm lifecycle policy drift was rejected")
    npm_tool_source = NPM_TOOLCHAIN_CHECK.read_text(encoding="utf-8")
    for label, current, drifted in [
        ("brace-expansion drift", '["brace-expansion", "5.0.9"]', '["brace-expansion", "5.0.7"]'),
        ("ip-address drift", '["ip-address", "10.3.1"]', '["ip-address", "10.2.0"]'),
    ]:
        mutated_tool = npm_tool_source.replace(current, drifted, 1)
        if not validate(source, makefile_source, security_source, npm_tool_source=mutated_tool):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    telemetry_supply = "\n".join(path.read_text(encoding="utf-8") for path in (
        COLLECTOR_DOCKERFILE, COLLECTOR_BUILDER, MOCK_DOCKERFILE, COLLECTOR_BUILD_CHECK, TELEMETRY_IMAGE_CHECK,
    ))
    for index, pin in enumerate(TELEMETRY_SUPPLY_PINS):
        mutated = telemetry_supply.replace(pin, f"telemetry-drift-{index}")
        if not validate(source, makefile_source, security_source, telemetry_supply_source=mutated):
            raise SystemExit(f"telemetry supply pin mutation was masked: {pin}")
        print(f"negative mutation passed: telemetry supply pin {index + 1} was rejected")
    python_pin_drift = telemetry_supply.replace("python3=3.14.7-r1", "python3=3.14.7-r0", 1)
    if not validate(source, makefile_source, security_source, telemetry_supply_source=python_pin_drift):
        raise SystemExit("telemetry Python package revision mutation was masked")
    print("negative mutation passed: telemetry Python package revision drift was rejected")
    module_drift = telemetry_supply.replace(" v0.158.0", " v0.157.0", 1)
    if not validate(source, makefile_source, security_source, telemetry_supply_source=module_drift):
        raise SystemExit("telemetry OCB module version mutation was masked")
    print("negative mutation passed: telemetry OCB module version drift was rejected")
    compile_drift = telemetry_supply.replace("builder --skip-compilation --config", "builder --config", 1)
    if not validate(source, makefile_source, security_source, telemetry_supply_source=compile_drift):
        raise SystemExit("telemetry OCB skip-compilation mutation was masked")
    print("negative mutation passed: telemetry OCB skip-compilation drift was rejected")
    manifest_drift = telemetry_supply + "\ndist:\n  skip_compilation: true\n"
    if not validate(source, makefile_source, security_source, telemetry_supply_source=manifest_drift):
        raise SystemExit("unsupported telemetry OCB manifest key mutation was masked")
    print("negative mutation passed: unsupported telemetry OCB manifest key was rejected")
    raise SystemExit(0)
errors = validate(source, makefile_source, security_source)
if errors:
    raise SystemExit("\n".join(errors))
print("workflow policy: required contexts, permissions, action SHAs, and service digests are fixed")
