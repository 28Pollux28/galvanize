#!/usr/bin/env python3
"""Log inbound TCP to published challenge ports (connect + first payload)."""
import json
import os
import socket
import struct
import time
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

DOCKER_SOCK = os.environ.get("DOCKER_SOCK", "/var/run/docker.sock")
IFACE = os.environ.get("CAPTURE_IFACE", "eth0")
LOG_PATH = Path(os.environ.get("LOG_PATH", "/var/log/tcp-access/access.log"))
MAX_PREVIEW = int(os.environ.get("MAX_PREVIEW", "256"))
MAX_EVENTS_PER_FLOW = int(os.environ.get("MAX_EVENTS_PER_FLOW", "8"))
FLOW_TTL = int(os.environ.get("FLOW_TTL", "120"))
PORT_REFRESH = float(os.environ.get("PORT_REFRESH", "4"))
SKIP_PORTS = {22, 80, 443, 8082, 8088, 2375, 2376, 9080, 3100}

ETH_P_ALL = 3
ETH_P_IP = 0x0800
ETH_P_IPV6 = 0x86DD


class DockerUnixConnection:
    def __init__(self, path):
        self.path = path

    def get(self, url):
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(5)
        sock.connect(self.path)
        try:
            req = f"GET {url} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
            sock.sendall(req.encode())
            buf = b""
            while True:
                chunk = sock.recv(65536)
                if not chunk:
                    break
                buf += chunk
        finally:
            sock.close()
        head, _, body = buf.partition(b"\r\n\r\n")
        if b" 200 " not in head.split(b"\r\n", 1)[0]:
            raise RuntimeError(head.split(b"\r\n", 1)[0].decode("latin1", "replace"))
        if b"chunked" in head.lower():
            body = _unchunk(body)
        return json.loads(body.decode())


def _unchunk(data):
    out = b""
    while data:
        line, _, rest = data.partition(b"\r\n")
        try:
            n = int(line.split(b";", 1)[0], 16)
        except ValueError:
            break
        if n == 0:
            break
        out += rest[:n]
        data = rest[n + 2 :]
    return out


def docker_ports():
    mapping = {}
    try:
        containers = DockerUnixConnection(DOCKER_SOCK).get("/containers/json?all=false")
    except Exception as exc:
        print(f"docker port map failed: {exc}", flush=True)
        return mapping
    for c in containers:
        name = (c.get("Names") or ["/unknown"])[0].lstrip("/")
        if not name.startswith("unitedctf-"):
            continue
        for p in c.get("Ports") or []:
            pub = p.get("PublicPort")
            if pub and p.get("Type", "tcp") == "tcp" and pub not in SKIP_PORTS:
                mapping[int(pub)] = name
    return mapping


def preview(data):
    text = "".join(chr(b) if 32 <= b < 127 or b in (9, 10, 13) else "." for b in data)
    return text.replace("\r", "\\r").replace("\n", "\\n")


def utcnow():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Writer:
    def __init__(self, path):
        path.parent.mkdir(parents=True, exist_ok=True)
        self.path = path
        self.fh = path.open("a", buffering=1)

    def emit(self, rec):
        self.fh.write(json.dumps(rec, separators=(",", ":")) + "\n")
        self.fh.flush()


def parse_ipv4(pkt):
    if len(pkt) < 20:
        return None
    ihl = (pkt[0] & 0x0F) * 4
    if pkt[0] >> 4 != 4 or pkt[9] != 6 or len(pkt) < ihl + 20:
        return None
    src = socket.inet_ntoa(pkt[12:16])
    return src, pkt[ihl:]


def parse_ipv6(pkt):
    if len(pkt) < 40 or pkt[6] != 6:
        return None
    src = socket.inet_ntop(socket.AF_INET6, pkt[8:24])
    return src, pkt[40:]


def parse_tcp(tcp):
    if len(tcp) < 20:
        return None
    sport, dport = struct.unpack_from("!HH", tcp, 0)
    offset = (tcp[12] >> 4) * 4
    flags = tcp[13]
    return sport, dport, flags, tcp[offset:]


def main():
    writer = Writer(LOG_PATH)
    ports = docker_ports()
    next_refresh = time.monotonic() + PORT_REFRESH
    flows = defaultdict(lambda: {"n": 0, "seen": 0.0})
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ALL))
    sock.bind((IFACE, 0))
    print(f"listening on {IFACE}, {len(ports)} challenge ports, log={LOG_PATH}", flush=True)
    while True:
        raw, _ = sock.recvfrom(2048)
        now = time.monotonic()
        if now >= next_refresh:
            ports = docker_ports()
            next_refresh = now + PORT_REFRESH
            dead = [k for k, v in flows.items() if now - v["seen"] > FLOW_TTL]
            for k in dead:
                del flows[k]
        if len(raw) < 14:
            continue
        eth = struct.unpack_from("!H", raw, 12)[0]
        if eth == ETH_P_IP:
            parsed = parse_ipv4(raw[14:])
        elif eth == ETH_P_IPV6:
            parsed = parse_ipv6(raw[14:])
        else:
            continue
        if not parsed:
            continue
        src, tcp = parsed
        parsed_tcp = parse_tcp(tcp)
        if not parsed_tcp:
            continue
        sport, dport, flags, payload = parsed_tcp
        if dport in SKIP_PORTS or dport not in ports:
            continue
        syn = bool(flags & 0x02) and not (flags & 0x10)
        psh = bool(flags & 0x08) and payload
        if not syn and not psh:
            continue
        key = (src, sport, dport)
        flow = flows[key]
        if flow["n"] >= MAX_EVENTS_PER_FLOW:
            continue
        flow["n"] += 1
        flow["seen"] = now
        data = payload[:MAX_PREVIEW]
        rec = {
            "time": utcnow(),
            "event": "connect" if syn and not payload else "data",
            "ClientHost": src,
            "ClientPort": sport,
            "DstPort": dport,
            "container": ports.get(dport, ""),
            "bytes": len(payload),
            "preview": preview(data),
            "hex": data.hex(),
        }
        writer.emit(rec)


if __name__ == "__main__":
    main()
