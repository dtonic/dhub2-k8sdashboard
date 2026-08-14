#!/usr/bin/env python3
"""Repository-owned policy checks for rendered Helm manifests."""
from __future__ import annotations

import argparse
import ipaddress
import re
import sys
from pathlib import Path


def require(ok: bool, message: str, errors: list[str]) -> None:
    if not ok:
        errors.append(message)


def check(text: str, environment: str) -> list[str]:
    errors: list[str] = []
    require("kind: Secret" not in text and "kind: ExternalSecret" not in text, "chart must not create secrets", errors)
    require("privileged: true" not in text, "privileged containers are forbidden", errors)
    require("allowPrivilegeEscalation: false" in text and "allowPrivilegeEscalation: true" not in text, "privilege escalation must be disabled", errors)
    # Every current Pod template has one container. Counting rendered images keeps
    # this policy correct when opt-in telemetry adds Agent/Gateway workloads.
    expected_containers = len(re.findall(r'^\s*image:\s*"?[^"\s]+"?\s*$', text, re.M))
    require(expected_containers >= 2, "rendered workload containers missing", errors)
    require(text.count('drop: ["ALL"]') >= expected_containers, "all containers must drop capabilities", errors)
    require("runAsNonRoot: true" in text and "readOnlyRootFilesystem: true" in text, "nonroot/read-only workload contexts required", errors)
    require('resources: ["pods", "nodes", "events"]' in text, "RBAC core allowlist drift", errors)
    require('resources: ["deployments", "statefulsets", "daemonsets", "replicasets"]' in text, "RBAC apps allowlist drift", errors)
    require('resources: ["cronjobs"]' in text and 'verbs: ["get", "list", "watch"]' in text, "RBAC verbs drift", errors)
    require(not re.search(r'resources:\s*\[[^\]]*"\*"|verbs:\s*\[[^\]]*"\*"|resources:\s*\[[^\]]*"secrets"', text), "wildcard/secret RBAC forbidden", errors)
    require("name: release-name-observability-dashboard-default-deny" in text, "default deny missing", errors)
    require(not re.search(r"\bfrom:\s*(?:null|\[\]|\{\})\s*$", text, re.M), "empty ingress source is forbidden", errors)
    require("kubernetes.io/metadata.name: kube-system" in text and "port: 53" in text, "bounded DNS egress missing", errors)
    for cidr in re.findall(r"\bcidr:\s*\"?([^\"}\s]+)", text):
        try:
            network = ipaddress.ip_network(cidr, strict=False)
            require(network.prefixlen != 0, f"broad CIDR forbidden: {cidr}", errors)
        except ValueError:
            errors.append(f"invalid CIDR: {cidr}")
    require("maxUnavailable: 0" in text and "maxSurge: 1" in text, "safe rolling update missing", errors)
    require(text.count("terminationGracePeriodSeconds: 30") >= expected_containers, "termination grace budget missing", errors)
    require("checksum/nginx:" in text and "checksum/config:" in text, "config rollout checksums missing", errors)
    require("path: /healthz" in text and "path: /readyz" in text and "startupProbe:" in text, "health/startup probes missing", errors)
    ingress_docs = re.findall(r"(?ms)^kind: Ingress\n.*?(?=^---\n|\Z)", text)
    require(all("-ui" in doc and "-api" not in doc for doc in ingress_docs), "Ingress may target UI only", errors)
    if environment in {"stage", "prod"}:
        images = re.findall(r'^\s*image:\s*"?([^"\s]+)"?\s*$', text, re.M)
        require(images and all("@sha256:" in image for image in images), "stage/prod images must be immutable digests", errors)
        require(text.count("kind: PodDisruptionBudget") >= 2, "stage/prod PDBs missing", errors)
        require(text.count("kind: HorizontalPodAutoscaler") >= 2, "stage/prod HPAs missing", errors)
        require('AUTH_MODE: "oidc"' in text and 'USE_DEMO_DATA: "false"' in text, "production auth/demo policy violated", errors)
        require("0.0.0.0/0" not in text and "::/0" not in text, "broad egress forbidden", errors)
    return errors


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--environment", choices=("dev", "stage", "prod"), required=True)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    text = args.manifest.read_text(encoding="utf-8")
    errors = check(text, args.environment)
    if args.self_test:
        mutations = {
            "privilege escalation": text.replace("allowPrivilegeEscalation: false", "allowPrivilegeEscalation: true", 1),
            "Secret creation": "apiVersion: v1\nkind: Secret\nmetadata: {name: forbidden}\n---\n" + text,
            "RBAC wildcard": text.replace('resources: ["pods", "nodes", "events"]', 'resources: ["*"]', 1),
            "invalid CIDR": re.sub(r"\bcidr:\s*\"?[^\"}\s]+", 'cidr: "999.999.1.1/33"', text, count=1),
            "broad CIDR": re.sub(r"\bcidr:\s*\"?[^\"}\s]+", 'cidr: "0.0.0.0/0"', text, count=1),
        }
        if args.environment in {"stage", "prod"}:
            mutations["mutable image"] = re.sub(r"@sha256:[a-f0-9]{64}", ":latest", text, count=1)
        for name, mutated in mutations.items():
            if not check(mutated, args.environment):
                errors.append(f"negative mutation was not rejected: {name}")
    if errors:
        print("deploy policy failed:\n- " + "\n- ".join(errors), file=sys.stderr)
        return 1
    print(f"deploy policy passed: {args.environment}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
