#!/usr/bin/env python3
import json
import os
import socket
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DOCKER_SOCK = os.environ.get("DOCKER_SOCK", "/var/run/docker.sock")
LISTEN = os.environ.get("LISTEN_ADDRESS", "0.0.0.0:8080")


class DockerUnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost")
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(5)
        self.sock.connect(self.path)


def docker(path):
    conn = DockerUnixConnection(DOCKER_SOCK)
    try:
        conn.request("GET", path)
        resp = conn.getresponse()
        body = resp.read()
        if resp.status >= 400:
            raise RuntimeError(f"docker {resp.status} {path}: {body[:200]!r}")
        return json.loads(body) if body else {}
    finally:
        conn.close()


def num(value):
    try:
        return float(value or 0)
    except (TypeError, ValueError):
        return 0.0


def collect():
    lines = [
        "# HELP docker_container_network_receive_bytes_total Network bytes received by the container",
        "# TYPE docker_container_network_receive_bytes_total counter",
        "# HELP docker_container_network_transmit_bytes_total Network bytes sent by the container",
        "# TYPE docker_container_network_transmit_bytes_total counter",
        "# HELP docker_container_memory_usage_bytes Memory usage of the container",
        "# TYPE docker_container_memory_usage_bytes gauge",
        "# HELP docker_container_cpu_usage_seconds_total Cumulative CPU time consumed",
        "# TYPE docker_container_cpu_usage_seconds_total counter",
    ]
    containers = docker("/containers/json?all=false")
    for c in containers:
        name = (c.get("Names") or ["/unknown"])[0].lstrip("/")
        labels = c.get("Labels") or {}
        project = labels.get("com.docker.compose.project", "")
        service = labels.get("com.docker.compose.service", "")
        cid = (c.get("Id") or "")[:12]
        try:
            stats = docker(f"/containers/{c['Id']}/stats?stream=false&one-shot=true")
        except Exception:
            continue
        rx = tx = 0.0
        for iface, net in (stats.get("networks") or {}).items():
            if iface in ("lo", "tunl0"):
                continue
            rx += num(net.get("rx_bytes"))
            tx += num(net.get("tx_bytes"))
        mem = num((stats.get("memory_stats") or {}).get("usage"))
        cpu = num(((stats.get("cpu_stats") or {}).get("cpu_usage") or {}).get("total_usage")) / 1e9
        label = (
            f'name="{name}",id="{cid}",'
            f'compose_project="{project}",compose_service="{service}"'
        )
        lines.append(f"docker_container_network_receive_bytes_total{{{label}}} {rx}")
        lines.append(f"docker_container_network_transmit_bytes_total{{{label}}} {tx}")
        lines.append(f"docker_container_memory_usage_bytes{{{label}}} {mem}")
        lines.append(f"docker_container_cpu_usage_seconds_total{{{label}}} {cpu}")
    return "\n".join(lines) + "\n"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        return

    def do_GET(self):
        if self.path not in ("/metrics", "/"):
            self.send_error(404)
            return
        try:
            body = collect().encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except Exception as exc:
            err = f"exporter_error {exc}\n".encode()
            self.send_response(500)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(err)))
            self.end_headers()
            self.wfile.write(err)


def main():
    host, port = LISTEN.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
