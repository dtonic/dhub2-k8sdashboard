#!/usr/bin/env python3
"""Validate the opt-in Alertmanager Helm wiring without exposing credentials."""

import argparse
import copy
import sys

import yaml


RBAC_KINDS = {"ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"}


def documents(path):
    with open(path, encoding="utf-8") as stream:
        return [doc for doc in yaml.safe_load_all(stream) if doc]


def one(docs, kind, suffix):
    matches = [doc for doc in docs if doc.get("kind") == kind and doc["metadata"]["name"].endswith(suffix)]
    assert len(matches) == 1, (kind, suffix, len(matches))
    return matches[0]


def validate(docs, baseline):
    assert not any(doc.get("kind") == "Secret" for doc in docs)
    deployment = one(docs, "Deployment", "-api")
    pod = deployment["spec"]["template"]["spec"]
    assert pod["securityContext"] == {
        "runAsNonRoot": True,
        "seccompProfile": {"type": "RuntimeDefault"},
        "fsGroup": 65532,
        "fsGroupChangePolicy": "OnRootMismatch",
    }
    container = next(item for item in pod["containers"] if item["name"] == "api")
    env = {item["name"]: item for item in container["env"]}
    expected = {
        "ALERTMANAGER_ENABLED": "true",
        "ALERTMANAGER_URL": "https://alerts.example.test:9443/am",
        "ALERTMANAGER_PUBLIC_URL": "https://alerts.example.test/am",
        "ALERTMANAGER_SERVER_NAME": "alerts.example.test",
        "ALERTMANAGER_CLUSTER_LABEL": "k8s_cluster_name",
        "ALERTMANAGER_NAMESPACE_LABEL": "namespace",
        "ALERTMANAGER_TIMEOUT": "5s",
        "ALERTMANAGER_MAX_BODY_BYTES": "4194304",
        "ALERTMANAGER_MAX_ALERTS": "2000",
        "ALERTMANAGER_MAX_CONCURRENT": "4",
        "ALERTMANAGER_TOKEN_FILE": "/var/run/alertmanager/token",
        "ALERTMANAGER_CA_FILE": "/var/run/alertmanager/ca.crt",
        "ALERTMANAGER_CLIENT_CERT_FILE": "/var/run/alertmanager/client.crt",
        "ALERTMANAGER_CLIENT_KEY_FILE": "/var/run/alertmanager/client.key",
    }
    for name, value in expected.items():
        assert env[name] == {"name": name, "value": value}, (name, env.get(name))
    assert all("valueFrom" not in env[name] for name in expected)
    assert next(item for item in container["volumeMounts"] if item["name"] == "alertmanager") == {
        "name": "alertmanager", "mountPath": "/var/run/alertmanager", "readOnly": True
    }
    volume = next(item for item in pod["volumes"] if item["name"] == "alertmanager")["secret"]
    assert volume["secretName"] == "alertmanager-client"
    assert volume["defaultMode"] == 0o440
    assert volume["items"] == [
        {"key": "bearer-token", "path": "token"},
        {"key": "ca.crt", "path": "ca.crt"},
        {"key": "tls.crt", "path": "client.crt"},
        {"key": "tls.key", "path": "client.key"},
    ]
    configmaps = [doc for doc in docs if doc.get("kind") == "ConfigMap"]
    assert not any("ALERTMANAGER" in str(doc.get("data", {})) for doc in configmaps)
    policy = one(docs, "NetworkPolicy", "-api")
    expected_egress = {"to": [{"ipBlock": {"cidr": "192.0.2.10/32"}}], "ports": [{"protocol": "TCP", "port": 9443}]}
    assert sum(item == expected_egress for item in policy["spec"]["egress"]) == 1
    if baseline:
        before = [doc for doc in baseline if doc.get("kind") in RBAC_KINDS]
        after = [doc for doc in docs if doc.get("kind") in RBAC_KINDS]
        assert before == after, "API ServiceAccount/RBAC changed when alerts were enabled"

    # Kubernetes projects the file as root:fsGroup. Prove group-read works for
    # the API identity while an unrelated uid/gid has no read bit.
    def can_read(mode, owner, group, uid, gids):
        if uid == owner:
            return bool(mode & 0o400)
        if group in gids:
            return bool(mode & 0o040)
        return bool(mode & 0o004)

    assert can_read(0o440, 0, 65532, 65532, {65532})
    assert not can_read(0o440, 0, 65532, 65531, {65531})


def self_test(docs, baseline):
    mutations = []
    mutated = copy.deepcopy(docs)
    api = one(mutated, "Deployment", "-api")["spec"]["template"]["spec"]["containers"][0]
    next(item for item in api["env"] if item["name"] == "ALERTMANAGER_TOKEN_FILE")["value"] = "raw-token"
    mutations.append(mutated)
    mutated = copy.deepcopy(docs)
    api = one(mutated, "Deployment", "-api")["spec"]["template"]["spec"]["containers"][0]
    next(item for item in api["volumeMounts"] if item["name"] == "alertmanager")["readOnly"] = False
    mutations.append(mutated)
    mutated = copy.deepcopy(docs)
    pod = one(mutated, "Deployment", "-api")["spec"]["template"]["spec"]
    next(item for item in pod["volumes"] if item["name"] == "alertmanager")["secret"]["defaultMode"] = 0o444
    mutations.append(mutated)
    mutated = copy.deepcopy(docs)
    pod = one(mutated, "Deployment", "-api")["spec"]["template"]["spec"]
    pod["securityContext"]["fsGroup"] = 65531
    mutations.append(mutated)
    mutated = copy.deepcopy(docs)
    policy = one(mutated, "NetworkPolicy", "-api")
    target = next(item for item in policy["spec"]["egress"] if item.get("to") == [{"ipBlock": {"cidr": "192.0.2.10/32"}}])
    target["ports"][0]["port"] = 443
    mutations.append(mutated)
    for index, mutation in enumerate(mutations):
        try:
            validate(mutation, baseline)
        except (AssertionError, KeyError, StopIteration):
            continue
        raise AssertionError(f"mutation {index} was not rejected")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest")
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    docs, baseline = documents(args.manifest), documents(args.baseline)
    validate(docs, baseline)
    if args.self_test:
        self_test(docs, baseline)
    print("alertmanager chart policy passed")


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, KeyError, StopIteration) as error:
        print(f"alertmanager chart policy failed: {error}", file=sys.stderr)
        raise SystemExit(1)
