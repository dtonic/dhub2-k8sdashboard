#!/usr/bin/env python3
"""Private-CA bearer proxy used only by the Alertmanager integration fixture."""

import argparse
import http.client
import json
import os
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


class State:
    def __init__(self, stats_path):
        self.path = stats_path
        self.lock = threading.Lock()
        self.requests = []

    def record(self, method, path, query):
        with self.lock:
            self.requests.append({"method": method, "path": path, "query": query})
            temporary = self.path + ".new"
            with open(temporary, "w", encoding="utf-8") as stream:
                json.dump({"requests": self.requests}, stream, separators=(",", ":"))
            os.replace(temporary, self.path)


def handler(upstream_host, upstream_port, token, state):
    class Proxy(BaseHTTPRequestHandler):
        server_version = "alertmanager-fixture"
        sys_version = ""

        def log_message(self, *_args):
            return

        def send_json(self, status, body):
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            parsed = urlsplit(self.path)
            state.record("GET", parsed.path, parsed.query)
            if self.headers.get("Authorization") != "Bearer " + token:
                self.send_json(401, b'{"error":"unauthorized"}')
                return
            if parsed.path == "/redirect/api/v2/alerts":
                self.send_response(302)
                self.send_header("Location", "https://redirect.invalid/api/v2/alerts")
                self.end_headers()
                return
            if parsed.path == "/oversize/api/v2/alerts":
                self.send_json(200, b"[" + (b" " * 70000) + b"]")
                return
            if parsed.path == "/timeout/api/v2/alerts":
                time.sleep(2)
                self.send_json(200, b"[]")
                return
            if parsed.path == "/outage/api/v2/alerts":
                self.send_json(503, b'{"error":"unavailable"}')
                return
            if parsed.path not in ("/am/api/v2/alerts", "/ignore/api/v2/alerts"):
                self.send_json(404, b'{"error":"not_found"}')
                return
            query = "" if parsed.path.startswith("/ignore/") else parsed.query
            target = "/api/v2/alerts" + (("?" + query) if query else "")
            connection = http.client.HTTPConnection(upstream_host, upstream_port, timeout=5)
            try:
                connection.request("GET", target, headers={"Accept": "application/json"})
                response = connection.getresponse()
                body = response.read(16 << 20)
                self.send_response(response.status)
                self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            finally:
                connection.close()

        def do_POST(self):
            parsed = urlsplit(self.path)
            state.record("POST", parsed.path, parsed.query)
            self.send_json(405, b'{"error":"read_only"}')

    return Proxy


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--upstream", required=True)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--token-file", required=True)
    parser.add_argument("--client-ca", required=True)
    parser.add_argument("--stats", required=True)
    parser.add_argument("--port-file", required=True)
    parser.add_argument("--container-network", action="store_true")
    args = parser.parse_args()
    upstream = urlsplit(args.upstream)
    allowed_upstream = ("alertmanager",) if args.container_network else ("127.0.0.1", "localhost")
    if upstream.scheme != "http" or upstream.hostname not in allowed_upstream or not upstream.port:
        raise SystemExit("upstream must be loopback HTTP")
    with open(args.token_file, encoding="ascii") as stream:
        token = stream.read().strip()
    state = State(args.stats)
    with open(args.stats, "w", encoding="utf-8") as stream:
        json.dump({"requests": []}, stream, separators=(",", ":"))
    listen = ("0.0.0.0", 9443) if args.container_network else ("127.0.0.1", 0)
    server = ThreadingHTTPServer(listen, handler(upstream.hostname, upstream.port, token, state))
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(args.cert, args.key)
    context.load_verify_locations(cafile=args.client_ca)
    context.verify_mode = ssl.CERT_REQUIRED
    server.socket = context.wrap_socket(server.socket, server_side=True)
    with open(args.port_file, "w", encoding="ascii") as stream:
        stream.write(str(server.server_address[1]))
    server.serve_forever()


if __name__ == "__main__":
    main()
