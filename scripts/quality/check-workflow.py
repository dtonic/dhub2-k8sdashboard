#!/usr/bin/env python3
import argparse
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "ci.yml"
MAKEFILE = ROOT / "Makefile"
SECURITY_SCAN = ROOT / "scripts" / "quality" / "security-scan.sh"
ACTION_PINS = {
    "actions/checkout": "11d5960a326750d5838078e36cf38b85af677262",
    "actions/setup-node": "49933ea5288caeca8642d1e84afbd3f7d6820020",
    "actions/setup-go": "40f1582b2485089dde7abd97c1529aa768e1baff",
    "actions/upload-artifact": "ea165f8d65b6e75b540449e92b4886f43607fa02",
}
REDIS = "redis:8.2.6-alpine@sha256:ea5a07305d6c66f99df5a5ff8d9659e8f6cb598e6e586dc8dd92b7fcd915746e"
GO_VERSION = "1.26.6"
GITLEAKS = "ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f"
TRIVY = "aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969"
GOVULN = "golang.org/x/vuln/cmd/govulncheck@v1.1.4"
REQUIRED_JOBS = {
    "Web (typecheck · build · e2e)",
    "API (vet · test · build)",
    "Deploy (images · Helm · schema · policy)",
}
REQUIRED_JOB_IDS = {"web", "api", "deploy"}

def validate(text, makefile, security_scan):
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
    ]
    for label, mutated_workflow, mutated_makefile, mutated_security in mutations:
        if not validate(mutated_workflow, mutated_makefile, mutated_security):
            raise SystemExit(f"{label} mutation was masked")
        print(f"negative mutation passed: {label} was rejected")
    raise SystemExit(0)
errors = validate(source, makefile_source, security_source)
if errors:
    raise SystemExit("\n".join(errors))
print("workflow policy: required contexts, permissions, action SHAs, and Redis digest are fixed")
