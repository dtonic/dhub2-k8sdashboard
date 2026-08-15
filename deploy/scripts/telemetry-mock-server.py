#!/usr/bin/env python3
"""Bounded same-network telemetry protocol fixture server."""

import base64
import http.server
import json
import threading
import time
import zlib

CADVISOR = b'''# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{namespace="payments",pod="api-0",container="api",image="must_drop",k8s_cluster_name="spoof"} 10
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{namespace="payments",pod="api-0",container="api",image="must_drop",k8s_cluster_name="spoof"} 1024
# TYPE container_network_receive_bytes_total counter
container_network_receive_bytes_total{namespace="payments",pod="api-0",container="api",interface="eth0",image="must_drop",k8s_cluster_name="spoof"} 20
# TYPE container_network_transmit_bytes_total counter
container_network_transmit_bytes_total{namespace="payments",pod="api-0",container="api",interface="eth0",image="must_drop",k8s_cluster_name="spoof"} 30
unbounded_not_catalogued{random_id="must_drop"} 1
'''
KSM = b'''# TYPE kube_pod_container_resource_requests gauge
kube_pod_container_resource_requests{namespace="payments",pod="api-0",container="api",resource="cpu",unit="core",uid="must_drop",k8s_cluster_name="spoof"} 1
kube_pod_container_resource_requests{namespace="payments",pod="api-0",container="api",resource="memory",unit="byte",uid="must_drop",k8s_cluster_name="spoof"} 1024
# TYPE kube_pod_container_status_restarts_total counter
kube_pod_container_status_restarts_total{namespace="payments",pod="api-0",container="api",uid="must_drop",k8s_cluster_name="spoof"} 2
unbounded_not_catalogued{random_id="must_drop"} 1
'''
real_scrapes = {"a": 0, "b": 0}


def real_cadvisor(cluster, tick):
    factor = 1 if cluster == "a" else 10
    return CADVISOR.replace(b'} 10\n', f'}} {10 + tick * factor}\n'.encode()).replace(
        b'} 1024\n', f'}} {1024 * factor}\n'.encode()).replace(
        b'} 20\n', f'}} {20 + tick * factor}\n'.encode()).replace(
        b'} 30\n', f'}} {30 + tick * factor}\n'.encode())


def real_ksm(cluster, tick):
    factor = 1 if cluster == "a" else 4
    restart_factor = 1 if cluster == "a" else 10
    return KSM.replace(b'} 1\n', f'}} {factor}\n'.encode()).replace(
        b'} 1024\n', f'}} {1024 * factor}\n'.encode()).replace(
        b'} 2\n', f'}} {2 + tick * restart_factor}\n'.encode())
MAX_BODY = 1_048_576
MAX_ITEMS = 128
MAX_TOTAL_BYTES = 8 * 1_048_576
lock = threading.Lock()
state = {
    "metrics": [], "logs": [], "headers": [], "failures_remaining": 1,
    "failures_seen": 0, "down_attempts": 0, "log_arrivals": [],
    "baseline_arrivals": [], "scrapes": {"/cadvisor": 0, "/ksm": 0},
}


class Handler(http.server.BaseHTTPRequestHandler):
    def send(self, status, body=b"", content_type="text/plain"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802
        if self.path == "/healthz":
            self.send(200, b"ok")
            return
        if self.path == "/__state":
            with lock:
                view = dict(state)
                view["scrapes"] = dict(state["scrapes"])
                view["metrics"] = [base64.b64encode(value).decode() for value in state["metrics"]]
                view["logs"] = [base64.b64encode(value).decode() for value in state["logs"]]
                view["headers"] = [dict(value) for value in state["headers"]]
                view["log_arrivals"] = list(state["log_arrivals"])
                view["baseline_arrivals"] = list(state["baseline_arrivals"])
            self.send(200, json.dumps(view, separators=(",", ":")).encode(), "application/json")
            return
        if self.path in ("/cadvisor-real", "/cadvisor-b-real", "/ksm-real", "/ksm-b-real"):
            cluster = "b" if "-b-" in self.path else "a"
            with lock:
                tick = real_scrapes[cluster]
                real_scrapes[cluster] += 1
            body = real_ksm(cluster, tick) if self.path.startswith("/ksm") else real_cadvisor(cluster, tick)
        elif self.path in state["scrapes"]:
            with lock:
                body = (CADVISOR if self.path == "/cadvisor" else KSM) if state["scrapes"][self.path] == 0 else b"# empty\n"
                state["scrapes"][self.path] += 1
        else:
            self.send(404)
            return
        self.send(200, body, "text/plain; version=0.0.4")

    def do_POST(self):  # noqa: N802
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send(400)
            return
        if length < 0 or length > MAX_BODY:
            self.send(413)
            return
        body = self.rfile.read(length)
        encoding = self.headers.get("Content-Encoding", "").lower()
        if encoding not in ("", "gzip"):
            self.send(415)
            return
        if encoding == "gzip":
            try:
                inflater = zlib.decompressobj(16 + zlib.MAX_WBITS)
                body = inflater.decompress(body, MAX_BODY + 1)
            except zlib.error:
                self.send(400)
                return
            if inflater.unconsumed_tail or not inflater.eof or len(body) > MAX_BODY:
                self.send(413)
                return
        if len(body) > MAX_BODY:
            self.send(413)
            return
        with lock:
            if self.path == "/baseline/v1/logs":
                if len(state["baseline_arrivals"]) >= MAX_ITEMS:
                    status = 507
                else:
                    state["baseline_arrivals"].append(time.time())
                    status = 200
            elif self.path == "/v1/metrics" and state["failures_remaining"]:
                state["failures_remaining"] -= 1
                state["failures_seen"] += 1
                status = 503
            elif self.path.startswith("/down/"):
                state["down_attempts"] += 1
                status = 503
            else:
                target = "metrics" if self.path == "/v1/metrics" else "logs" if self.path == "/v1/logs" else None
                captured_bytes = sum(map(len, state["metrics"])) + sum(map(len, state["logs"]))
                if target is None or len(state[target]) >= MAX_ITEMS or captured_bytes + len(body) > MAX_TOTAL_BYTES:
                    status = 404 if target is None else 507
                else:
                    state[target].append(body)
                    if target == "logs":
                        state["log_arrivals"].append(time.time())
                    state["headers"].append({key.lower(): value for key, value in self.headers.items()})
                    status = 200
        self.send(status, content_type="application/x-protobuf")

    def log_message(self, *_):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
