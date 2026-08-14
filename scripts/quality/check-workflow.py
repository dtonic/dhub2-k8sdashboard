#!/usr/bin/env python3
import argparse
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "ci.yml"
MAKEFILE = ROOT / "Makefile"
SECURITY_SCAN = ROOT / "scripts" / "quality" / "security-scan.sh"
ROOT_PACKAGE = ROOT / "package.json"
WEB_DOCKERFILE = ROOT / "Dockerfile.web"
API_DOCKERFILE = ROOT / "Dockerfile.api"
GO_WORK = ROOT / "go.work"
API_GO_MOD = ROOT / "apps" / "api" / "go.mod"
README = ROOT / "README.md"
NPM_TOOLCHAIN_CHECK = ROOT / "scripts" / "quality" / "check-npm-toolchain.mjs"
ACTION_PINS = {
    "actions/checkout": "11d5960a326750d5838078e36cf38b85af677262",
    "actions/setup-node": "49933ea5288caeca8642d1e84afbd3f7d6820020",
    "actions/setup-go": "40f1582b2485089dde7abd97c1529aa768e1baff",
    "actions/upload-artifact": "ea165f8d65b6e75b540449e92b4886f43607fa02",
}
REDIS = "redis:8.2.6-alpine@sha256:ea5a07305d6c66f99df5a5ff8d9659e8f6cb598e6e586dc8dd92b7fcd915746e"
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
GOVULN = "golang.org/x/vuln/cmd/govulncheck@v1.1.4"
REQUIRED_JOBS = {
    "Web (typecheck · build · e2e)",
    "API (vet · test · build)",
    "Deploy (images · Helm · schema · policy)",
}
REQUIRED_JOB_IDS = {"web", "api", "deploy"}

def validate(text, makefile, security_scan, package_source=None, docker_source=None, npm_tool_source=None,
             go_work_source=None, go_mod_source=None, api_docker_source=None, readme_source=None):
    errors = []
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
    if service_images != [REDIS]:
        errors.append("Redis service image is not pinned to the approved manifest digest")
    go_versions = re.findall(r'(?m)^\s+go-version:\s*["\']?([^"\'\s]+)', text)
    if go_versions != [GO_VERSION]:
        errors.append("setup-go version is not fixed to the approved patch")
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
        ("mutable govulncheck", source, makefile_source.replace(GOVULN, "golang.org/x/vuln/cmd/govulncheck@latest", 1), security_source),
        ("mutable gitleaks", source, makefile_source, security_source.replace(GITLEAKS, "ghcr.io/gitleaks/gitleaks:v8.30.1", 1)),
        ("mutable Trivy", source, makefile_source, security_source.replace(TRIVY, "aquasec/trivy:0.74.0", 1)),
        ("job rename masked by step", source.replace(next(iter(REQUIRED_JOBS)), "Renamed job", 1) + f"\n      - name: {next(iter(REQUIRED_JOBS))}\n        run: true\n", makefile_source, security_source),
        ("extra job", source + "\n  unexpected:\n    name: Unexpected quality job\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("unnamed extra job", source + "\n  unnamed-extra:\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("duplicate web override", source + "\n  web:\n    runs-on: ubuntu-latest\n    steps: []\n", makefile_source, security_source),
        ("Go version drift", source.replace(f'go-version: "{GO_VERSION}"', 'go-version: "1.26.7"', 1), makefile_source, security_source),
        ("Node version drift", source.replace(f"node-version: {NODE_VERSION}", "node-version: 22.23.1", 1), makefile_source, security_source),
    ]
    for label, mutated_workflow, mutated_makefile, mutated_security in mutations:
        if not validate(mutated_workflow, mutated_makefile, mutated_security):
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
    raise SystemExit(0)
errors = validate(source, makefile_source, security_source)
if errors:
    raise SystemExit("\n".join(errors))
print("workflow policy: required contexts, permissions, action SHAs, and Redis digest are fixed")
