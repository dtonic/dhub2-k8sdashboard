#!/usr/bin/env python3
"""Deterministic Collector protocol test; uses only pinned repo-owned images."""
from __future__ import annotations

import argparse
import base64
import gzip
import hashlib
import json
import math
import os
import re
import shlex
import socket
import struct
import subprocess
import tempfile
import time
import urllib.request
import urllib.parse
import urllib.error
import uuid
from datetime import datetime, timezone
from pathlib import Path

COLLECTOR = os.environ.get("TELEMETRY_COLLECTOR_IMAGE")
GREPTIME = "greptime/greptimedb@sha256:9726587eac95d0360755254cd59a528dbf48abfdf268478aea6a644f62afe44c"
QUICKWIT = "quickwit/quickwit@sha256:1e6169bf4e98a489fca397105f1698c1d80a0f9779f3cf652973bac8a0c3b2bd"
METRICS = {
    "container_cpu_usage_seconds_total": "sum",
    "container_memory_working_set_bytes": "gauge",
    "container_network_receive_bytes_total": "sum",
    "container_network_transmit_bytes_total": "sum",
    "kube_pod_container_resource_requests": "gauge",
    "kube_pod_container_status_restarts_total": "sum",
}
OWNER = os.environ.get("TELEMETRY_TEST_OWNER", "issue23")
if not re.fullmatch(r"issue[0-9]+", OWNER):
    raise SystemExit("TELEMETRY_TEST_OWNER must match issue[0-9]+")

def run(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args, check=check, text=True, encoding="utf-8", errors="replace",
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )


def fields(data: bytes):
    pos = 0
    while pos < len(data):
        key, pos = varint(data, pos)
        number, wire = key >> 3, key & 7
        if wire == 0:
            value, pos = varint(data, pos)
        elif wire == 1:
            value, pos = data[pos:pos + 8], pos + 8
        elif wire == 2:
            size, pos = varint(data, pos)
            value, pos = data[pos:pos + size], pos + size
        elif wire == 5:
            value, pos = data[pos:pos + 4], pos + 4
        else:
            raise AssertionError(f"unsupported protobuf wire type {wire}")
        yield number, wire, value


def varint(data: bytes, pos: int) -> tuple[int, int]:
    value = shift = 0
    while True:
        byte = data[pos]
        pos += 1
        value |= (byte & 0x7F) << shift
        if byte < 0x80:
            return value, pos
        shift += 7


def parse_metrics(payload: bytes) -> list[tuple[str, str, set[str]]]:
    out: list[tuple[str, str, set[str]]] = []
    for n, w, resource in fields(payload):
        if n != 1 or w != 2:
            continue
        for rn, rw, scope in fields(resource):
            if rn != 2 or rw != 2:
                continue
            for sn, sw, metric in fields(scope):
                if sn != 2 or sw != 2:
                    continue
                name = ""
                kind = ""
                labels: set[str] = set()
                for mn, mw, value in fields(metric):
                    if mn == 1 and mw == 2:
                        name = value.decode()
                    elif mn in (5, 7) and mw == 2:
                        kind = "gauge" if mn == 5 else "sum"
                        for dn, dw, point in fields(value):
                            if dn == 1 and dw == 2:
                                for pn, pw, attribute in fields(point):
                                    if pn == 7 and pw == 2:
                                        for kn, kw, key in fields(attribute):
                                            if kn == 1 and kw == 2:
                                                labels.add(key.decode())
                if name:
                    out.append((name, kind, labels))
    return out


def string_attribute(payload: bytes) -> tuple[str, str]:
    key = decoded = ""
    for number, wire, value in fields(payload):
        if number == 1 and wire == 2:
            key = value.decode()
        elif number == 2 and wire == 2:
            for value_number, value_wire, scalar in fields(value):
                if value_number == 1 and value_wire == 2:
                    decoded = scalar.decode()
    return key, decoded


def string_attributes(payload: bytes) -> dict[str, str]:
    out: dict[str, str] = {}
    for number, wire, value in fields(payload):
        if number == 1 and wire == 2:
            key, decoded = string_attribute(value)
            if key:
                out[key] = decoded
    return out


def metric_cluster_attributes(payload: bytes) -> list[tuple[str, dict[str, str], dict[str, str]]]:
    out: list[tuple[str, dict[str, str], dict[str, str]]] = []
    for number, wire, resource_metrics in fields(payload):
        if number != 1 or wire != 2:
            continue
        resource_attributes: dict[str, str] = {}
        scopes: list[bytes] = []
        for rm_number, rm_wire, rm_value in fields(resource_metrics):
            if rm_number == 1 and rm_wire == 2:
                resource_attributes = string_attributes(rm_value)
            elif rm_number == 2 and rm_wire == 2:
                scopes.append(rm_value)
        for scope in scopes:
            for scope_number, scope_wire, metric in fields(scope):
                if scope_number != 2 or scope_wire != 2:
                    continue
                name = ""
                point_attributes: dict[str, str] = {}
                for metric_number, metric_wire, metric_value in fields(metric):
                    if metric_number == 1 and metric_wire == 2:
                        name = metric_value.decode()
                    elif metric_number in (5, 7) and metric_wire == 2:
                        for data_number, data_wire, point in fields(metric_value):
                            if data_number == 1 and data_wire == 2:
                                for point_number, point_wire, attribute in fields(point):
                                    if point_number == 7 and point_wire == 2:
                                        key, decoded = string_attribute(attribute)
                                        if key:
                                            point_attributes[key] = decoded
                if name:
                    out.append((name, resource_attributes, point_attributes))
    return out


def collector_stats(name: str) -> dict[str, object]:
    payload = json.loads(run("docker", "stats", "--no-stream", "--format", "{{json .}}", name).stdout)
    memory = re.match(r"([0-9.]+)([KMG]iB)", payload["MemUsage"])
    if not memory:
        raise AssertionError(f"unparseable Docker memory stat: {payload}")
    scale = {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024}[memory.group(2)]
    return {
        "name": name.split("-", 2)[1],
        "cpuPercent": float(payload["CPUPerc"].rstrip("%")),
        "memoryMiB": float(memory.group(1)) * scale,
    }


def collector_cpu_snapshot(name: str, helper_image: str) -> dict[str, int]:
    helper = f"{OWNER}-cpu-{uuid.uuid4().hex[:12]}"
    program = (
        "import json,os,pathlib\n"
        "def runtime(path):\n"
        " try: return int(path.read_text().split()[0])\n"
        " except FileNotFoundError: return 0\n"
        "fields=pathlib.Path('/proc/1/stat').read_text().split()\n"
        "total=sum(runtime(path) for path in pathlib.Path('/proc/1/task').glob('*/schedstat'))\n"
        "print(json.dumps({'statMicros':(int(fields[13])+int(fields[14]))*1000000//os.sysconf('SC_CLK_TCK'),"
        "'runtimeNanos':total}))"
    )
    if run("docker", "inspect", helper, check=False).returncode == 0:
        raise AssertionError(f"CPU helper name collision: {helper}")
    value = json.loads(run(
        "docker", "run", "--rm", "--name", helper, "--network", "none",
        "--pid", f"container:{name}", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges", "--entrypoint", "python3", helper_image,
        "-B", "-c", program,
    ).stdout.strip())
    if set(value) != {"statMicros", "runtimeNanos"} or min(value.values()) < 0:
        raise AssertionError(f"invalid collector CPU snapshot: {value}")
    return value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence-out", type=Path)
    args = parser.parse_args()
    token = uuid.uuid4().hex[:12]
    collector = COLLECTOR or f"{OWNER}-otelcol-{token}:local"
    collector_owned = COLLECTOR is None
    if collector_owned:
        if run("docker", "image", "inspect", collector, check=False).returncode == 0:
            raise AssertionError(f"collector image tag collision: {collector}")
        repository = Path(__file__).resolve().parents[2]
        run("docker", "build", "--quiet", "-f", str(repository / "deploy/telemetry/Dockerfile.collector"),
            "-t", collector, str(repository / "deploy/telemetry"))
    elif run("docker", "image", "inspect", collector, check=False).returncode:
        raise AssertionError(f"pre-verified collector image is missing: {collector}")
    run_started = time.monotonic()
    started_at = datetime.now(timezone.utc)
    network = f"{OWNER}-otel-{token}"
    mock = f"{OWNER}-mock-{token}"
    mock_image = f"{OWNER}-mock-image-{token}:local"
    gateway = f"{OWNER}-gateway-{token}"
    source = f"{OWNER}-source-{token}"
    greptime = f"{OWNER}-greptime-{token}"
    greptime_source = f"{OWNER}-greptime-source-{token}"
    down_collector = f"{OWNER}-down-{token}"
    baseline_collector = f"{OWNER}-baseline-{token}"
    quickwit = f"{OWNER}-quickwit-{token}"
    quickwit_indexer = f"{OWNER}-quickwit-index-{token}"
    quickwit_source = f"{OWNER}-quickwit-source-{token}"
    owner_label = f"{OWNER}.owner={token}"
    owned_containers: dict[str, str] = {}
    persistent_containers = (
        mock, gateway, source, down_collector, baseline_collector, greptime,
        greptime_source, quickwit, quickwit_source,
    )
    fixture_host = mock
    port = 8080
    with tempfile.TemporaryDirectory(prefix=f"{OWNER}-otel-") as tmp:
        root = Path(tmp)
        gateway_cfg = root / "gateway.yaml"
        source_cfg = root / "source.yaml"
        greptime_cfg = root / "greptime-source.yaml"
        down_cfg = root / "down.yaml"
        baseline_cfg = root / "baseline.yaml"
        quickwit_cfg = root / "quickwit-source.yaml"
        gateway_cfg.write_text(f"""
receivers:
  otlp:
    protocols:
      grpc: {{endpoint: 0.0.0.0:4317}}
processors:
  transform/schema:
    error_mode: propagate
    metric_statements:
      - context: datapoint
        statements:
          - set(attributes["k8s_cluster_name"], resource.attributes["k8s.cluster.name"]) where resource.attributes["k8s.cluster.name"] != nil
          - keep_keys(attributes, ["container", "interface", "k8s_cluster_name", "namespace", "pod", "resource", "unit"])
    log_statements:
      - context: log
        statements:
          - 'replace_pattern(body, "(?i)(password|token)[=: ]+[^ ,;]+", "[REDACTED_SECRET]") where IsString(body)'
          - set(cache["message"], body) where IsString(body)
          - set(body, cache) where IsString(body)
exporters:
  otlp_http/mock:
    endpoint: http://{fixture_host}:{port}
    headers:
      x-greptime-otlp-metric-translation-strategy: NoTranslation
    sending_queue: {{enabled: true, queue_size: 16, num_consumers: 1}}
    retry_on_failure: {{enabled: true, initial_interval: 100ms, max_interval: 200ms, max_elapsed_time: 1s}}
service:
  pipelines:
    metrics: {{receivers: [otlp], processors: [transform/schema], exporters: [otlp_http/mock]}}
    logs: {{receivers: [otlp], processors: [transform/schema], exporters: [otlp_http/mock]}}
""", encoding="utf-8")
        source_cfg.write_text(f"""
receivers:
  otlp:
    protocols:
      http: {{endpoint: 0.0.0.0:4318}}
  prometheus/catalog:
    config:
      scrape_configs:
        - job_name: cadvisor
          scrape_interval: 2s
          metrics_path: /cadvisor
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'container_cpu_usage_seconds_total|container_memory_working_set_bytes|container_network_receive_bytes_total|container_network_transmit_bytes_total', action: keep}}
        - job_name: kube-state-metrics
          scrape_interval: 2s
          metrics_path: /ksm
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'kube_pod_container_resource_requests|kube_pod_container_status_restarts_total', action: keep}}
processors:
  resource/cluster:
    attributes:
      - {{key: k8s.cluster.name, value: cluster-a, action: upsert}}
  filter/catalog:
    error_mode: propagate
    metrics:
      metric:
        - 'not (name == "container_cpu_usage_seconds_total" or name == "container_memory_working_set_bytes" or name == "container_network_receive_bytes_total" or name == "container_network_transmit_bytes_total" or name == "kube_pod_container_resource_requests" or name == "kube_pod_container_status_restarts_total")'
exporters:
  otlp_grpc/gateway: {{endpoint: {gateway}:4317, tls: {{insecure: true}}}}
service:
  pipelines:
    metrics: {{receivers: [prometheus/catalog], processors: [resource/cluster, filter/catalog], exporters: [otlp_grpc/gateway]}}
    logs: {{receivers: [otlp], exporters: [otlp_grpc/gateway]}}
""", encoding="utf-8")
        greptime_cfg.write_text(f"""
receivers:
  prometheus/a:
    config:
      scrape_configs:
        - job_name: cadvisor
          scrape_interval: 1s
          metrics_path: /cadvisor-real
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'container_cpu_usage_seconds_total|container_memory_working_set_bytes|container_network_receive_bytes_total|container_network_transmit_bytes_total', action: keep}}
        - job_name: kube-state-metrics
          scrape_interval: 1s
          metrics_path: /ksm-real
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'kube_pod_container_resource_requests|kube_pod_container_status_restarts_total', action: keep}}
  prometheus/b:
    config:
      scrape_configs:
        - job_name: cadvisor-b
          scrape_interval: 1s
          metrics_path: /cadvisor-b-real
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'container_cpu_usage_seconds_total|container_memory_working_set_bytes|container_network_receive_bytes_total|container_network_transmit_bytes_total', action: keep}}
        - job_name: kube-state-metrics-b
          scrape_interval: 1s
          metrics_path: /ksm-b-real
          static_configs: [{{targets: ["{fixture_host}:{port}"]}}]
          metric_relabel_configs:
            - {{source_labels: [__name__], regex: 'kube_pod_container_resource_requests|kube_pod_container_status_restarts_total', action: keep}}
processors:
  resource/cluster-a:
    attributes:
      - {{key: k8s.cluster.name, value: cluster-a, action: upsert}}
  resource/cluster-b:
    attributes:
      - {{key: k8s.cluster.name, value: cluster-b, action: upsert}}
  transform/cluster:
    error_mode: propagate
    metric_statements:
      - context: datapoint
        statements:
          - set(attributes["k8s_cluster_name"], resource.attributes["k8s.cluster.name"])
  filter/catalog:
    error_mode: propagate
    metrics:
      metric:
        - 'not (name == "container_cpu_usage_seconds_total" or name == "container_memory_working_set_bytes" or name == "container_network_receive_bytes_total" or name == "container_network_transmit_bytes_total" or name == "kube_pod_container_resource_requests" or name == "kube_pod_container_status_restarts_total")'
exporters:
  otlp_http/greptime:
    endpoint: http://{greptime}:4000/v1/otlp
    headers:
      X-Greptime-DB-Name: public
      x-greptime-otlp-metric-translation-strategy: NoTranslation
service:
  pipelines:
    metrics/a: {{receivers: [prometheus/a], processors: [resource/cluster-a, filter/catalog, transform/cluster], exporters: [otlp_http/greptime]}}
    metrics/b: {{receivers: [prometheus/b], processors: [resource/cluster-b, filter/catalog, transform/cluster], exporters: [otlp_http/greptime]}}
""", encoding="utf-8")
        quickwit_cfg.write_text(f"""
receivers:
  otlp:
    protocols:
      http: {{endpoint: 0.0.0.0:4319}}
processors:
  transform/privacy:
    error_mode: propagate
    log_statements:
      - context: log
        statements:
          - 'replace_pattern(body, "(?i)(password|passwd|pwd|secret|token|authorization)[=: ]+[^ ,;]+", "[REDACTED_SECRET]") where IsString(body)'
          - 'replace_pattern(body, "[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+[.][A-Za-z]{{2,}}", "[REDACTED_EMAIL]") where IsString(body)'
          - 'replace_pattern(body, "(?:[0-9][ -]*?){{13,19}}", "[REDACTED_CARD]") where IsString(body)'
          - 'replace_pattern(body, "(?:[0-9A-Fa-f]{{1,4}}:){{7}}[0-9A-Fa-f]{{1,4}}|(?:[0-9A-Fa-f]{{1,4}}:){{1,7}}:|(?:[0-9A-Fa-f]{{1,4}}:){{1,6}}:[0-9A-Fa-f]{{1,4}}|(?:[0-9A-Fa-f]{{1,4}}:){{1,5}}(?::[0-9A-Fa-f]{{1,4}}){{1,2}}|(?:[0-9A-Fa-f]{{1,4}}:){{1,4}}(?::[0-9A-Fa-f]{{1,4}}){{1,3}}|(?:[0-9A-Fa-f]{{1,4}}:){{1,3}}(?::[0-9A-Fa-f]{{1,4}}){{1,4}}|(?:[0-9A-Fa-f]{{1,4}}:){{1,2}}(?::[0-9A-Fa-f]{{1,4}}){{1,5}}|[0-9A-Fa-f]{{1,4}}:(?:(?::[0-9A-Fa-f]{{1,4}}){{1,6}})|:(?:(?::[0-9A-Fa-f]{{1,4}}){{1,7}}|:)", "[REDACTED_IP]") where IsString(body)'
          - 'replace_pattern(body, "(?:[0-9]{{1,3}}[.]){{3}}[0-9]{{1,3}}", "[REDACTED_IP]") where IsString(body)'
          - set(cache["message"], body) where IsString(body)
          - set(body, cache) where IsString(body)
          - keep_keys(attributes, ["event_id"])
exporters:
  otlp_grpc/quickwit:
    endpoint: {quickwit}:7281
    tls: {{insecure: true}}
    headers: {{qw-otel-logs-index: otel-logs-v0_7}}
    sending_queue: {{enabled: true, queue_size: 16, num_consumers: 1}}
    retry_on_failure: {{enabled: true, initial_interval: 250ms, max_interval: 2s, max_elapsed_time: 30s}}
service:
  pipelines:
    logs: {{receivers: [otlp], processors: [transform/privacy], exporters: [otlp_grpc/quickwit]}}
""", encoding="utf-8")
        down_cfg.write_text(f"""
receivers:
  otlp:
    protocols:
      http: {{endpoint: 0.0.0.0:4320}}
exporters:
  otlp_http/down:
    endpoint: http://{fixture_host}:{port}/down
    sending_queue: {{enabled: true, queue_size: 2, num_consumers: 1, block_on_overflow: false}}
    retry_on_failure: {{enabled: true, initial_interval: 100ms, max_interval: 200ms, max_elapsed_time: 1s}}
service:
  pipelines:
    logs: {{receivers: [otlp], exporters: [otlp_http/down]}}
""", encoding="utf-8")
        baseline_cfg.write_text(f"""
receivers:
  otlp:
    protocols:
      http: {{endpoint: 0.0.0.0:4321}}
exporters:
  otlp_http/baseline:
    endpoint: http://{fixture_host}:{port}/baseline
    sending_queue: {{enabled: true, queue_size: 16, num_consumers: 1}}
    retry_on_failure: {{enabled: true, initial_interval: 100ms, max_interval: 200ms, max_elapsed_time: 1s}}
service:
  pipelines:
    logs: {{receivers: [otlp], exporters: [otlp_http/baseline]}}
""", encoding="utf-8")
        for name in (*persistent_containers, quickwit_indexer):
            if run("docker", "inspect", name, check=False).returncode == 0:
                raise AssertionError(f"container name collision: {name}")
        if run("docker", "network", "inspect", network, check=False).returncode == 0:
            raise AssertionError(f"network name collision: {network}")
        run("docker", "network", "create", "--label", owner_label, network)
        try:
            repository = Path(__file__).resolve().parents[2]
            run("docker", "build", "--quiet", "-f", str(repository / "deploy/telemetry/Dockerfile.mock"),
                "-t", mock_image, str(repository))
            run("docker", "run", "-d", "--name", mock, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::8080",
                "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16m",
                "--user", "65532:65532", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges",
                mock_image)
            owned_containers[mock] = run("docker", "inspect", "-f", "{{.Id}}", mock).stdout.strip()
            mock_port = run("docker", "port", mock, "8080/tcp").stdout.strip().rsplit(":", 1)[1]
            mock_url = f"http://127.0.0.1:{mock_port}"
            for _ in range(40):
                try:
                    urllib.request.urlopen(f"{mock_url}/healthz", timeout=1).read()
                    break
                except OSError:
                    time.sleep(0.1)
            else:
                print(run("docker", "logs", mock, check=False).stdout[-4000:])
                raise AssertionError("same-network mock did not become healthy")
            assert run("docker", "exec", mock, "python3", "--version").stdout.strip() == "Python 3.14.7"
            security = json.loads(run("docker", "inspect", mock).stdout)[0]
            assert security["HostConfig"]["ReadonlyRootfs"] is True
            assert security["HostConfig"]["CapDrop"] == ["ALL"]
            assert "no-new-privileges" in security["HostConfig"]["SecurityOpt"]

            bad_encoding = urllib.request.Request(
                f"{mock_url}/v1/logs", data=b"x", headers={"Content-Encoding": "br"},
            )
            try:
                urllib.request.urlopen(bad_encoding, timeout=2)
                raise AssertionError("mock accepted unsupported content encoding")
            except urllib.error.HTTPError as exc:
                assert exc.code == 415
            gzip_bomb = urllib.request.Request(
                f"{mock_url}/v1/logs", data=gzip.compress(b"x" * 1_048_577),
                headers={"Content-Encoding": "gzip"},
            )
            try:
                urllib.request.urlopen(gzip_bomb, timeout=2)
                raise AssertionError("mock accepted oversized decompressed body")
            except urllib.error.HTTPError as exc:
                assert exc.code == 413
            with socket.create_connection(("127.0.0.1", int(mock_port)), timeout=2) as conn:
                conn.sendall(b"POST /v1/logs HTTP/1.1\r\nHost: fixture\r\nContent-Length: nope\r\n\r\n")
                assert b" 400 " in conn.recv(128)

            def mock_state() -> dict[str, object]:
                value = json.loads(urllib.request.urlopen(f"{mock_url}/__state", timeout=2).read())
                value["metrics"] = [base64.b64decode(item) for item in value["metrics"]]
                value["logs"] = [base64.b64decode(item) for item in value["logs"]]
                return value

            run("docker", "run", "-d", "--name", gateway, "--label", owner_label, "--network", network,
                "--mount", f"type=bind,source={gateway_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[gateway] = run("docker", "inspect", "-f", "{{.Id}}", gateway).stdout.strip()
            run("docker", "run", "-d", "--name", source, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::4318",
                "--mount", f"type=bind,source={source_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[source] = run("docker", "inspect", "-f", "{{.Id}}", source).stdout.strip()
            mapped = run("docker", "port", source, "4318/tcp").stdout.strip().rsplit(":", 1)[1]
            log = {"resourceLogs": [{"scopeLogs": [{"logRecords": [{
                "timeUnixNano": "1720000000000000000", "severityText": "ERROR",
                "body": {"stringValue": "password=hunter2 token=abcdefghijklmnop"}
            }]}]}]}
            req = urllib.request.Request(f"http://127.0.0.1:{mapped}/v1/logs", data=json.dumps(log).encode(),
                                         headers={"Content-Type": "application/json"})
            for _ in range(20):
                try:
                    urllib.request.urlopen(req, timeout=2).read()
                    break
                except OSError:
                    time.sleep(0.25)
            deadline = time.time() + 20
            seen: set[str] = set()
            while time.time() < deadline:
                state = mock_state()
                seen = {name for payload in state["metrics"] for name, _, _ in parse_metrics(payload)}
                if state["logs"] and set(METRICS) <= seen:
                    break
                time.sleep(0.25)
            state = mock_state()
            print(f"captured metrics={len(state['metrics'])} logs={len(state['logs'])} headers={len(state['headers'])}")
            assert state["metrics"] and state["logs"], "collector did not route both signals"
            parsed = [item for payload in state["metrics"] for item in parse_metrics(payload)]
            assert {name for name, _, _ in parsed} == set(METRICS), f"unexpected metric escaped catalog filter: {parsed}"
            counts = {name: sum(1 for got, _, _ in parsed if got == name) for name in METRICS}
            assert counts == {name: 1 for name in METRICS}, f"duplicate/missing metric samples: {counts}"
            assert state["failures_seen"] == 1, f"expected one injected 503, got {state['failures_seen']}"
            assert run("docker", "inspect", "-f", "{{.State.Running}}", gateway).stdout.strip() == "true"
            for name, kind, labels in parsed:
                assert kind == METRICS[name], f"{name}: type {kind}"
                assert {"namespace", "pod", "container"} <= labels, f"{name}: required labels {labels}"
                assert not ({"image", "uid", "random_id"} & labels), f"{name}: unbounded labels {labels}"
            cluster_details = [item for payload in state["metrics"] for item in metric_cluster_attributes(payload)]
            assert cluster_details, "metric cluster attributes missing"
            for name, resource_attrs, point_attrs in cluster_details:
                assert resource_attrs.get("k8s.cluster.name") == "cluster-a", f"{name}: resource cluster identity drift: {resource_attrs}"
                assert point_attrs.get("k8s_cluster_name") == "cluster-a", f"{name}: datapoint cluster label missing/spoofed: {point_attrs}"
            raw_logs = b"".join(state["logs"])
            assert b"hunter2" not in raw_logs and b"abcdefghijklmnop" not in raw_logs
            assert b"REDACTED_SECRET" in raw_logs and b"message" in raw_logs
            assert any(h.get("x-greptime-otlp-metric-translation-strategy") == "NoTranslation" for h in state["headers"])
            print(f"protocol fixture passed: metrics={counts} log_bytes={len(raw_logs)} queue_size=16")

            run("docker", "run", "-d", "--name", down_collector, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::4320",
                "--mount", f"type=bind,source={down_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[down_collector] = run("docker", "inspect", "-f", "{{.Id}}", down_collector).stdout.strip()
            down_port = run("docker", "port", down_collector, "4320/tcp").stdout.strip().rsplit(":", 1)[1]
            down_req = urllib.request.Request(
                f"http://127.0.0.1:{down_port}/v1/logs", data=json.dumps(log).encode(),
                headers={"Content-Type": "application/json"},
            )
            for _ in range(40):
                try:
                    urllib.request.urlopen(down_req, timeout=2).read()
                    break
                except OSError:
                    time.sleep(0.25)
            else:
                raise AssertionError("permanent-failure collector receiver did not become ready")
            bounded_started = time.monotonic()
            time.sleep(2)
            bounded_elapsed = time.monotonic() - bounded_started
            state = mock_state()
            assert 2 <= state["down_attempts"] <= 20, f"unbounded/missing retry attempts: {state['down_attempts']}"
            assert run("docker", "inspect", "-f", "{{.State.Running}}", down_collector).stdout.strip() == "true"
            assert "queue_size: 2" in down_cfg.read_text() and "max_elapsed_time: 1s" in down_cfg.read_text()
            print(f"permanent 503 isolation passed: attempts={state['down_attempts']} elapsed_seconds={bounded_elapsed:.2f} queue_size=2")

            run("docker", "run", "-d", "--name", baseline_collector, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::4321",
                "--mount", f"type=bind,source={baseline_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[baseline_collector] = run("docker", "inspect", "-f", "{{.Id}}", baseline_collector).stdout.strip()
            baseline_port = run("docker", "port", baseline_collector, "4321/tcp").stdout.strip().rsplit(":", 1)[1]
            baseline_stats: list[dict[str, object]] = []
            candidate_stats: list[list[dict[str, object]]] = []
            baseline_cpu_time_nanos: list[int] = []
            candidate_cpu_time_nanos: list[int] = []
            baseline_cpu_stat_micros: list[int] = []
            candidate_cpu_stat_micros: list[int] = []
            cpu_trial_duration_ms: list[int] = []
            baseline_latencies: list[int] = []
            candidate_latencies: list[int] = []
            candidate_measurement_started = 0.0
            candidate_measurement_ended = 0.0
            corpus_count = 30
            canonical_corpus = json.dumps(log, sort_keys=True, separators=(",", ":"))
            corpus_event_digests = [
                hashlib.sha256(f"{index}:{canonical_corpus}".encode()).hexdigest()
                for index in range(corpus_count)
            ]
            comparison_payload_before = sum(map(len, state["logs"]))
            baseline_req = urllib.request.Request(
                f"http://127.0.0.1:{baseline_port}/v1/logs", data=json.dumps(log).encode(),
                headers={"Content-Type": "application/json"},
            )
            for _ in range(40):
                try:
                    urllib.request.urlopen(baseline_req, timeout=2).read()
                    break
                except OSError:
                    time.sleep(0.25)
            else:
                raise AssertionError("baseline collector receiver did not become ready")
            deadline = time.monotonic() + 3
            state = mock_state()
            while not state["baseline_arrivals"] and time.monotonic() < deadline:
                time.sleep(0.01)
                state = mock_state()
            assert state["baseline_arrivals"], "baseline warmup was not exported"
            for index in range(corpus_count):
                if index % 10 == 0:
                    baseline_cpu_started = collector_cpu_snapshot(baseline_collector, mock_image)
                    source_cpu_started = collector_cpu_snapshot(source, mock_image)
                    gateway_cpu_started = collector_cpu_snapshot(gateway, mock_image)
                    cpu_trial_started = time.monotonic()
                state = mock_state()
                baseline_before = len(state["baseline_arrivals"])
                started = time.time()
                urllib.request.urlopen(baseline_req, timeout=2).read()
                deadline = time.monotonic() + 3
                state = mock_state()
                while len(state["baseline_arrivals"]) == baseline_before and time.monotonic() < deadline:
                    time.sleep(0.01)
                    state = mock_state()
                assert len(state["baseline_arrivals"]) == baseline_before + 1, "baseline log was not exported exactly once"
                baseline_latencies.append(round((state["baseline_arrivals"][-1] - started) * 1000))
                before = len(state["logs"])
                if not candidate_latencies:
                    candidate_measurement_started = time.monotonic()
                started = time.time()
                urllib.request.urlopen(req, timeout=2).read()
                deadline = time.monotonic() + 3
                state = mock_state()
                while len(state["logs"]) == before and time.monotonic() < deadline:
                    time.sleep(0.01)
                    state = mock_state()
                assert len(state["logs"]) == before + 1, "candidate log was not exported exactly once"
                candidate_latencies.append(round((state["log_arrivals"][-1] - started) * 1000))
                candidate_measurement_ended = time.monotonic()
                if (index + 1) % 10 == 0:
                    cpu_trial_ended = time.monotonic()
                    # Sample immediately after each trial so resource evidence covers
                    # the measured workload rather than the pre-trial idle state.
                    baseline_stats.append(collector_stats(baseline_collector))
                    candidate_stats.append([collector_stats(source), collector_stats(gateway)])
                    baseline_cpu_ended = collector_cpu_snapshot(baseline_collector, mock_image)
                    source_cpu_ended = collector_cpu_snapshot(source, mock_image)
                    gateway_cpu_ended = collector_cpu_snapshot(gateway, mock_image)
                    baseline_cpu_time_nanos.append(baseline_cpu_ended["runtimeNanos"] - baseline_cpu_started["runtimeNanos"])
                    candidate_cpu_time_nanos.append(
                        source_cpu_ended["runtimeNanos"] - source_cpu_started["runtimeNanos"]
                        + gateway_cpu_ended["runtimeNanos"] - gateway_cpu_started["runtimeNanos"]
                    )
                    baseline_cpu_stat_micros.append(baseline_cpu_ended["statMicros"] - baseline_cpu_started["statMicros"])
                    candidate_cpu_stat_micros.append(
                        source_cpu_ended["statMicros"] - source_cpu_started["statMicros"]
                        + gateway_cpu_ended["statMicros"] - gateway_cpu_started["statMicros"]
                    )
                    cpu_trial_duration_ms.append(max(1, round((cpu_trial_ended - cpu_trial_started) * 1000)))

            def p95(values: list[int]) -> int:
                return sorted(values)[math.ceil(len(values) * 0.95) - 1]

            baseline_p95 = p95(baseline_latencies)
            candidate_p95 = p95(candidate_latencies)
            baseline_trial_p95 = [p95(baseline_latencies[offset:offset + 10]) for offset in range(0, corpus_count, 10)]
            candidate_trial_p95 = [p95(candidate_latencies[offset:offset + 10]) for offset in range(0, corpus_count, 10)]
            candidate_memory_samples = [sum(float(sample["memoryMiB"]) for sample in trial) for trial in candidate_stats]
            baseline_memory_mib = math.ceil(max(float(sample["memoryMiB"]) for sample in baseline_stats))
            peak_memory_mib = math.ceil(max(candidate_memory_samples))
            assert all(value > 0 for value in baseline_cpu_time_nanos + candidate_cpu_time_nanos), (
                f"collector cumulative CPU did not advance: baseline={baseline_cpu_time_nanos} candidate={candidate_cpu_time_nanos}"
            )
            baseline_cpu_total = sum(baseline_cpu_time_nanos)
            candidate_cpu_total = sum(candidate_cpu_time_nanos)
            cpu_observation_ms = sum(cpu_trial_duration_ms)
            baseline_cpu_millicores = math.ceil(baseline_cpu_total / (cpu_observation_ms * 1000))
            candidate_cpu_millicores = math.ceil(candidate_cpu_total / (cpu_observation_ms * 1000))

            run("docker", "run", "-d", "--name", greptime, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::4000", GREPTIME, "standalone", "start",
                "--http-addr=0.0.0.0:4000", "--grpc-bind-addr=0.0.0.0:4001",
                "--mysql-addr=0.0.0.0:4002", "--postgres-addr=0.0.0.0:4003",
                "--data-home=/tmp/greptime")
            owned_containers[greptime] = run("docker", "inspect", "-f", "{{.Id}}", greptime).stdout.strip()
            greptime_port = run("docker", "port", greptime, "4000/tcp").stdout.strip().rsplit(":", 1)[1]
            health = f"http://127.0.0.1:{greptime_port}/health"
            for _ in range(160):
                try:
                    urllib.request.urlopen(health, timeout=1).read()
                    break
                except OSError:
                    time.sleep(0.25)
            else:
                raise AssertionError("GreptimeDB did not become healthy")
            greptime_storage_before = int(run("docker", "exec", greptime, "du", "-sb", "/tmp/greptime").stdout.split()[0])
            run("docker", "run", "-d", "--name", greptime_source, "--label", owner_label, "--network", network,
                "--mount", f"type=bind,source={greptime_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[greptime_source] = run("docker", "inspect", "-f", "{{.Id}}", greptime_source).stdout.strip()
            sql_url = f"http://127.0.0.1:{greptime_port}/v1/sql?" + urllib.parse.urlencode({"sql": "show tables"})
            sql_req = urllib.request.Request(sql_url, headers={"X-Greptime-DB-Name": "public"})
            visible_started = time.monotonic()
            tables: dict[str, object] = {}
            for _ in range(80):
                try:
                    tables = json.loads(urllib.request.urlopen(sql_req, timeout=2).read())
                    if all(name.encode() in json.dumps(tables).encode() for name in METRICS):
                        break
                except OSError:
                    pass
                time.sleep(0.25)
            else:
                print(run("docker", "logs", greptime_source, check=False).stdout[-5000:])
                print(run("docker", "logs", greptime, check=False).stdout[-5000:])
                raise AssertionError(f"Greptime metric visibility timeout: {tables}")
            visibility_seconds = time.monotonic() - visible_started
            print(f"Greptime tables: {tables}")
            table_names = {row[0] for row in tables["output"][0]["records"]["rows"]}
            # Greptime creates this internal physical table independently of OTLP input.
            expected_table_names = set(METRICS) | {"greptime_physical_table"}
            assert table_names == expected_table_names, f"unexpected Greptime tables: {table_names}"
            metric_table_names = table_names - {"greptime_physical_table"}
            assert metric_table_names == set(METRICS), f"unexpected Greptime metric tables: {metric_table_names}"
            assert "up" not in metric_table_names and not any(name.startswith("scrape_") for name in metric_table_names)
            queries = {
                "metrics.cpu.used": 'sum by (k8s_cluster_name) (rate(container_cpu_usage_seconds_total{k8s_cluster_name="$cluster",namespace="payments",container!=""}[1m]))',
                "metrics.cpu.requested": 'sum by (k8s_cluster_name) (kube_pod_container_resource_requests{k8s_cluster_name="$cluster",namespace="payments",resource="cpu"})',
                "metrics.memory.used": 'sum by (k8s_cluster_name) (container_memory_working_set_bytes{k8s_cluster_name="$cluster",namespace="payments",container!=""})',
                "metrics.memory.requested": 'sum by (k8s_cluster_name) (kube_pod_container_resource_requests{k8s_cluster_name="$cluster",namespace="payments",resource="memory"})',
                "metrics.network.rx": 'sum by (k8s_cluster_name) (rate(container_network_receive_bytes_total{k8s_cluster_name="$cluster",namespace="payments"}[1m]))',
                "metrics.network.tx": 'sum by (k8s_cluster_name) (rate(container_network_transmit_bytes_total{k8s_cluster_name="$cluster",namespace="payments"}[1m]))',
                "metrics.restarts": 'sum by (k8s_cluster_name) (increase(kube_pod_container_status_restarts_total{k8s_cluster_name="$cluster",namespace="payments"}[1m]))',
                "metrics.usage.cpu_milli": '1000 * sum by (k8s_cluster_name, namespace, pod) (rate(container_cpu_usage_seconds_total{k8s_cluster_name="$cluster",container!=""}[1m]))',
                "metrics.usage.memory_mib": 'sum by (k8s_cluster_name, namespace, pod) (container_memory_working_set_bytes{k8s_cluster_name="$cluster",container!=""}) / 1048576',
            }
            query_started = time.monotonic()
            missing: dict[str, object] = {}
            scoped_results: dict[tuple[str, str], dict[str, object]] = {}
            for _ in range(120):
                missing.clear()
                for cluster in ("cluster-a", "cluster-b"):
                    for ref, template in queries.items():
                        query = template.replace("$cluster", cluster)
                        assert query.count(f'k8s_cluster_name="{cluster}"') == 1, (ref, query)
                        url = f"http://127.0.0.1:{greptime_port}/v1/prometheus/api/v1/query?" + urllib.parse.urlencode({"query": query})
                        req = urllib.request.Request(url, headers={"X-Greptime-DB-Name": "public"})
                        try:
                            result = json.loads(urllib.request.urlopen(req, timeout=2).read())
                        except (OSError, ValueError) as exc:
                            missing[f"{cluster}/{ref}"] = str(exc)
                            continue
                        rows = result.get("data", {}).get("result", [])
                        if result.get("status") != "success" or not rows:
                            missing[f"{cluster}/{ref}"] = result
                        else:
                            scoped_results[(cluster, ref)] = rows[0]
                if not missing:
                    break
                time.sleep(0.25)
            else:
                print(run("docker", "logs", greptime_source, check=False).stdout[-5000:])
                print(run("docker", "logs", greptime, check=False).stdout[-5000:])
                raise AssertionError(f"Greptime query visibility timeout: {missing}")
            for cluster in ("cluster-a", "cluster-b"):
                for ref in queries:
                    metric = scoped_results[(cluster, ref)].get("metric", {})
                    assert metric.get("k8s_cluster_name") == cluster, f"{cluster}/{ref}: response identity drift: {metric}"
            for ref in queries:
                a_value = float(scoped_results[("cluster-a", ref)]["value"][1])
                b_value = float(scoped_results[("cluster-b", ref)]["value"][1])
                assert a_value != b_value, f"{ref}: A/B values merged: {a_value}"
            a_memory = float(scoped_results[("cluster-a", "metrics.memory.used")]["value"][1])
            b_memory = float(scoped_results[("cluster-b", "metrics.memory.used")]["value"][1])
            assert a_memory == 1024 and b_memory == 10240, f"cross-cluster metric merge: A={a_memory} B={b_memory}"
            query_visibility = time.monotonic() - query_started
            print(f"Greptime v1.1.4 A/B round trip passed: scoped_query_refs={len(queries) * 2} A_memory={a_memory} B_memory={b_memory} table_visibility_seconds={visibility_seconds:.2f} query_visibility_seconds={query_visibility:.2f}")

            run("docker", "run", "-d", "--name", quickwit, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::7280", "-e", "QW_ENABLE_OTLP_ENDPOINT=true",
                QUICKWIT, "run")
            owned_containers[quickwit] = run("docker", "inspect", "-f", "{{.Id}}", quickwit).stdout.strip()
            quickwit_port = run("docker", "port", quickwit, "7280/tcp").stdout.strip().rsplit(":", 1)[1]
            ready = f"http://127.0.0.1:{quickwit_port}/health/readyz"
            for _ in range(120):
                try:
                    urllib.request.urlopen(ready, timeout=1).read()
                    break
                except OSError:
                    time.sleep(0.25)
            else:
                print(run("docker", "logs", quickwit, check=False).stdout[-5000:])
                raise AssertionError("Quickwit did not become ready")
            index_config_path = Path(__file__).resolve().parents[1] / "telemetry" / "quickwit-otel-logs-v0_7.yaml"
            run("docker", "run", "--rm", "--name", quickwit_indexer, "--network", network,
                "--mount", f"type=bind,source={index_config_path},target=/conf/index.yaml,readonly",
                QUICKWIT, "index", "create", "--endpoint", f"http://{quickwit}:7280",
                "--index-config", "/conf/index.yaml", "--yes")
            index_url = f"http://127.0.0.1:{quickwit_port}/api/v1/indexes/otel-logs-v0_7"
            index_config: dict[str, object] = {}
            for _ in range(80):
                try:
                    index_config = json.loads(urllib.request.urlopen(index_url, timeout=2).read())
                    if index_config.get("index_config", index_config).get("index_id") == "otel-logs-v0_7":
                        break
                except (OSError, ValueError, AttributeError):
                    pass
                time.sleep(0.25)
            else:
                print(run("docker", "logs", quickwit, check=False).stdout[-5000:])
                raise AssertionError(f"Quickwit exact index readiness timeout: {index_config}")

            def mapped_field(node: object, name: str) -> dict[str, object] | None:
                if isinstance(node, dict):
                    if node.get("name") == name:
                        return node
                    for value in node.values():
                        found = mapped_field(value, name)
                        if found:
                            return found
                elif isinstance(node, list):
                    for value in node:
                        found = mapped_field(value, name)
                        if found:
                            return found
                return None

            for field_name, field_type in (("timestamp_nanos", "datetime"), ("attributes", "json"), ("resource_attributes", "json")):
                mapping = mapped_field(index_config, field_name)
                fast = mapping.get("fast") if mapping else None
                assert mapping and mapping.get("type") == field_type and (fast is True or isinstance(fast, dict)), (
                    f"Quickwit OTLP schema drift for {field_name}: {mapping}"
                )
                if field_type == "json":
                    assert mapping.get("tokenizer") == "raw" and mapping.get("expand_dots") is True, (
                        f"Quickwit JSON field drift for {field_name}: {mapping}"
                    )
            quickwit_storage_before = int(run("docker", "exec", quickwit, "du", "-sb", "/quickwit/qwdata").stdout.split()[0])
            run("docker", "run", "-d", "--name", quickwit_source, "--label", owner_label, "--network", network,
                "-p", "127.0.0.1::4319",
                "--mount", f"type=bind,source={quickwit_cfg},target=/conf/config.yaml,readonly",
                collector, "--config=/conf/config.yaml")
            owned_containers[quickwit_source] = run("docker", "inspect", "-f", "{{.Id}}", quickwit_source).stdout.strip()
            quickwit_ingest_port = run("docker", "port", quickwit_source, "4319/tcp").stdout.strip().rsplit(":", 1)[1]
            now_ns = time.time_ns()
            resource_logs = []
            log_fixtures = (("cluster-a", "payments"), ("cluster-a", "payments"), ("cluster-b", "payments"), ("cluster-b", "payments"), ("cluster-a", "other"))
            for index, (cluster, namespace) in enumerate(log_fixtures, start=1):
                resource = {
                    "attributes": [
                        {"key": "k8s.cluster.name", "value": {"stringValue": cluster}},
                        {"key": "k8s.namespace.name", "value": {"stringValue": namespace}},
                        {"key": "k8s.pod.name", "value": {"stringValue": "checkout-same"}},
                        {"key": "k8s.pod.uid", "value": {"stringValue": "pod-same"}},
                        {"key": "k8s.container.name", "value": {"stringValue": "api"}},
                        {"key": "k8s.node.name", "value": {"stringValue": "node-a"}},
                        {"key": "k8s.workload.kind", "value": {"stringValue": "Deployment"}},
                        {"key": "k8s.workload.name", "value": {"stringValue": "checkout"}},
                        {"key": "service.name", "value": {"stringValue": "checkout-api"}},
                    ]
                }
                record = {
                    "timeUnixNano": str(now_ns + index), "severityText": "ERROR",
                    "traceId": "0123456789abcdef0123456789abcdef",
                    "spanId": "0123456789abcdef",
                    "body": {"stringValue": "password=hunter2 token=abcdefghijklmnop card=4111 1111 1111 1111 email=user@example.com ipv4=10.20.30.40 ipv6=fd12:3456:789a:1::42"},
                    "attributes": [{"key": "event_id", "value": {"stringValue": f"event-{cluster}-{index}"}}],
                }
                resource_logs.append({"resource": resource, "scopeLogs": [{"logRecords": [record]}]})
            otlp_req = urllib.request.Request(
                f"http://127.0.0.1:{quickwit_ingest_port}/v1/logs",
                data=json.dumps({"resourceLogs": resource_logs}).encode(),
                headers={"Content-Type": "application/json"},
            )
            for _ in range(40):
                try:
                    urllib.request.urlopen(otlp_req, timeout=2).read()
                    break
                except OSError:
                    time.sleep(0.25)
            else:
                print(run("docker", "logs", quickwit_source, check=False).stdout[-5000:])
                raise AssertionError("Quickwit source collector OTLP receiver did not become ready")
            search_url = f"http://127.0.0.1:{quickwit_port}/api/v1/otel-logs-v0_7/search?query=*"
            quickwit_visible_started = time.monotonic()
            visible_docs = 0
            for _ in range(120):
                try:
                    result = json.loads(urllib.request.urlopen(search_url, timeout=2).read())
                    visible_docs = int(result.get("num_hits", 0))
                    if visible_docs >= len(log_fixtures):
                        break
                except (OSError, ValueError):
                    pass
                time.sleep(0.25)
            else:
                print(run("docker", "logs", quickwit_source, check=False).stdout[-5000:])
                print(run("docker", "logs", quickwit, check=False).stdout[-5000:])
                raise AssertionError(f"Quickwit OTLP visibility timeout: hits={visible_docs}")
            quickwit_visibility = time.monotonic() - quickwit_visible_started
            stored = json.dumps(result).encode()
            for sensitive in (b"hunter2", b"abcdefghijklmnop", b"4111 1111", b"user@example.com", b"10.20.30.40", b"fd12:3456:789a:1::42"):
                assert sensitive not in stored, f"raw sensitive value persisted in Quickwit: {sensitive!r}"
            for marker in (b"REDACTED_SECRET", b"REDACTED_CARD", b"REDACTED_EMAIL", b"REDACTED_IP"):
                assert marker in stored, f"expected redaction marker absent from Quickwit: {marker!r}"
            assert stored.count(b"[REDACTED_IP]") == 2 * len(log_fixtures), "IPv4 and IPv6 must use the same marker in every document"
            api_dir = Path(__file__).resolve().parents[2] / "apps" / "api"
            env = dict(os.environ)
            env.update({"QUICKWIT_ITEST_URL": f"http://127.0.0.1:{quickwit_port}", "QUICKWIT_OTEL_SCHEMA": "1"})
            if os.name == "nt":
                api_posix = f"/mnt/{api_dir.drive[0].lower()}/" + "/".join(api_dir.parts[1:])
                test_command = (
                    f"cd {shlex.quote(api_posix)} && "
                    f"QUICKWIT_ITEST_URL={shlex.quote(env['QUICKWIT_ITEST_URL'])} QUICKWIT_OTEL_SCHEMA=1 "
                    "go test -tags=integration ./internal/datasource/quickwit "
                    "-run TestLiveQuickwitOTLPSchemaCompatibility -count=1"
                )
                api_test = subprocess.run(
                    ["wsl", "bash", "-lc", test_command], text=True, encoding="utf-8", errors="replace",
                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                )
            else:
                api_test = subprocess.run(
                    ["go", "test", "-tags=integration", "./internal/datasource/quickwit", "-run", "TestLiveQuickwitOTLPSchemaCompatibility", "-count=1"],
                    cwd=api_dir, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                )
            if api_test.returncode:
                print(api_test.stdout)
                raise AssertionError("Quickwit BFF OTLP schema integration failed")
            for cluster in ("cluster-a", "cluster-b"):
                scoped_url = f"http://127.0.0.1:{quickwit_port}/api/v1/otel-logs-v0_7/search?" + urllib.parse.urlencode({"query": f'resource_attributes.k8s.cluster.name:{cluster} AND resource_attributes.k8s.namespace.name:payments'})
                scoped = json.loads(urllib.request.urlopen(scoped_url, timeout=2).read())
                assert int(scoped.get("num_hits", 0)) == 2, f"Quickwit cluster isolation failed for {cluster}: {scoped}"
                encoded_scoped = json.dumps(scoped)
                other = "cluster-b" if cluster == "cluster-a" else "cluster-a"
                assert f"event-{cluster}-" in encoded_scoped and f"event-{other}-" not in encoded_scoped
            print(f"Quickwit v0.9.0 OTLP/BFF A/B passed: docs={visible_docs} scoped_hits=2+2 visibility_seconds={quickwit_visibility:.2f}")
            greptime_storage = max(0, int(run("docker", "exec", greptime, "du", "-sb", "/tmp/greptime").stdout.split()[0]) - greptime_storage_before)
            quickwit_storage = max(0, int(run("docker", "exec", quickwit, "du", "-sb", "/quickwit/qwdata").stdout.split()[0]) - quickwit_storage_before)
            baseline_events = len(baseline_latencies)
            candidate_events = len(candidate_latencies)
            loss_permille = max(0, (baseline_events - candidate_events) * 1000 // baseline_events)
            candidate_measurement_duration_ms = max(1, round((candidate_measurement_ended - candidate_measurement_started) * 1000))
            events_per_hour = math.ceil(candidate_events * 3_600_000 / candidate_measurement_duration_ms)
            retention_days = 7
            price_micros_per_gib_month = 23_000
            replication_factor = 1
            observed_storage = greptime_storage + quickwit_storage
            observed_stored_events = len(METRICS) + visible_docs
            storage_bytes_per_day = math.ceil(observed_storage / observed_stored_events * events_per_hour * 24 * replication_factor)
            cost_micros_per_day = math.ceil(
                storage_bytes_per_day * retention_days * price_micros_per_gib_month / (1024**3 * 30)
            )
            state = mock_state()
            payload_bytes = sum(map(len, state["logs"])) - comparison_payload_before
            egress_bytes_per_hour = math.ceil(payload_bytes / candidate_events * events_per_hour)
            raw = {
                "comparisonScope": "synthetic-otlp-hop",
                "baselineTopology": "pinned-source-collector->mock",
                "candidateTopology": "pinned-source-collector->gateway-transform->mock",
                "corpusDigest": hashlib.sha256(
                    json.dumps(corpus_event_digests, separators=(",", ":")).encode()
                ).hexdigest(),
                "corpusEventDigests": corpus_event_digests,
                "corpusCount": corpus_count,
                "baselineEvents": baseline_events,
                "candidateEvents": candidate_events,
                "duplicates": sum(max(0, count - 1) for count in counts.values()),
                "injected503": state["failures_seen"],
                "permanent503Attempts": state["down_attempts"],
                "payloadBytes": payload_bytes,
                "quickwitDocuments": visible_docs,
                "baselineLatenciesMs": baseline_latencies,
                "candidateLatenciesMs": candidate_latencies,
                "baselineTrialEventCounts": [10, 10, 10],
                "candidateTrialEventCounts": [10, 10, 10],
                "baselineTrialP95Ms": baseline_trial_p95,
                "candidateTrialP95Ms": candidate_trial_p95,
                "candidateMeasurementDurationMs": candidate_measurement_duration_ms,
                "cpuTrialDurationMs": cpu_trial_duration_ms,
                "baselineCollectorSamples": baseline_stats,
                "candidateCollectorSamples": candidate_stats,
                "baselineCpuMillicores": baseline_cpu_millicores,
                "baselineMemoryMiB": baseline_memory_mib,
                "cpuDeltaMillicores": candidate_cpu_millicores - baseline_cpu_millicores,
                "memoryDeltaMiB": peak_memory_mib - baseline_memory_mib,
                "baselineCpuTimeNanos": baseline_cpu_time_nanos,
                "candidateCpuTimeNanos": candidate_cpu_time_nanos,
                "baselineCpuStatMicros": baseline_cpu_stat_micros,
                "candidateCpuStatMicros": candidate_cpu_stat_micros,
                "cpuTimeDeltaNanos": sum(candidate_cpu_time_nanos) - sum(baseline_cpu_time_nanos),
                "baselineCpuNanosPerEvent": math.ceil(sum(baseline_cpu_time_nanos) / baseline_events),
                "candidateCpuNanosPerEvent": math.ceil(sum(candidate_cpu_time_nanos) / candidate_events),
                "storedBytes": {"greptime": greptime_storage, "quickwit": quickwit_storage},
                "observedStoredEvents": observed_stored_events,
                "assumptions": {
                    "eventsPerHour": events_per_hour,
                    "retentionDays": retention_days,
                    "replicationFactor": replication_factor,
                    "priceMicrosPerGiBMonth": price_micros_per_gib_month,
                    "currency": "USD",
                    "priceUnit": "GiB-month",
                    "priceSource": "local-explicit-assumption-not-market-quote",
                },
            }
            ended_at = datetime.now(timezone.utc)
            evidence = {
                "schemaVersion": 1,
                "environment": "local",
                "kind": "local-synthetic-comparison",
                "startedAt": started_at.isoformat().replace("+00:00", "Z"),
                "endedAt": ended_at.isoformat().replace("+00:00", "Z"),
                "windowMinutes": max(1, math.ceil((ended_at - started_at).total_seconds() / 60)),
                "raw": raw,
                "lossPermille": loss_permille,
                "greptimeTableVisibilityMs": round(visibility_seconds * 1000),
                "greptimeAllQueryVisibilityMs": round(query_visibility * 1000),
                "quickwitVisibilityMs": round(quickwit_visibility * 1000),
                "endToEndRuntimeMs": round((time.monotonic() - run_started) * 1000),
                "p95LatencyMs": candidate_p95,
                "collectorCpuMillicores": candidate_cpu_millicores,
                "collectorMemoryMiB": peak_memory_mib,
                "egressBytesPerHour": egress_bytes_per_hour,
                "storageBytesPerDay": storage_bytes_per_day,
                "estimatedCostMicrosPerDay": cost_micros_per_day,
                "operatorProductionMeasurementsRequired": True,
            }
            evidence["artifactHash"] = hashlib.sha256(
                json.dumps(evidence, sort_keys=True, separators=(",", ":")).encode()
            ).hexdigest()
            assert evidence["lossPermille"] == 0 and evidence["raw"]["duplicates"] == 0
            assert evidence["raw"]["payloadBytes"] <= 1_048_576
            assert candidate_p95 <= 3_000
            assert max(evidence["greptimeTableVisibilityMs"], evidence["greptimeAllQueryVisibilityMs"], evidence["quickwitVisibilityMs"]) <= 30_000
            encoded_evidence = json.dumps(evidence, sort_keys=True, separators=(",", ":"))
            if args.evidence_out:
                args.evidence_out.write_text(encoded_evidence + "\n", encoding="utf-8")
            print(f"EVIDENCE_JSON={encoded_evidence}")
        finally:
            try:
                state = mock_state()
            except Exception:
                state = {"metrics": [], "logs": []}
            if not state["metrics"] or not state["logs"]:
                print(run("docker", "logs", source, check=False).stdout[-4000:])
                print(run("docker", "logs", gateway, check=False).stdout[-4000:])
            for name in persistent_containers:
                current = run("docker", "inspect", "-f", "{{.Id}}", name, check=False)
                if name in owned_containers and current.returncode == 0 and current.stdout.strip() == owned_containers[name]:
                    run("docker", "rm", "-f", name, check=False)
            label = run("docker", "network", "inspect", "-f", f'{{{{index .Labels "{OWNER}.owner"}}}}', network, check=False)
            if label.returncode == 0 and label.stdout.strip() == token:
                run("docker", "network", "rm", network, check=False)
            run("docker", "image", "rm", mock_image, check=False)
            if collector_owned:
                run("docker", "image", "rm", collector, check=False)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
