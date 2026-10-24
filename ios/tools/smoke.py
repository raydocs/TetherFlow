#!/usr/bin/env python3
"""Authenticated smoke/diagnostic client. No system proxy changes or third-party packages."""
import argparse
import getpass
import hashlib
import json
import os
import socket
import ssl
import time
from urllib.parse import urlsplit

MAX_BYTES = 256 * 1024 * 1024


def read_header(stream):
    lines = []
    size = 0
    while True:
        line = stream.readline(16 * 1024 + 1)
        size += len(line)
        if not line or size > 16 * 1024 or not line.endswith(b"\r\n"):
            raise ValueError("invalid/oversized/incomplete HTTP response")
        if line == b"\r\n":
            break
        lines.append(line[:-2])
    if not lines:
        raise ValueError("missing HTTP status line")
    status = lines[0].decode("ascii")
    pieces = status.split(" ", 2)
    if len(pieces) < 2 or pieces[0] != "HTTP/1.1" or len(pieces[1]) != 3 or not pieces[1].isdigit():
        raise ValueError("invalid HTTP status line")
    headers = {}
    for line in lines[1:]:
        name, value = line.decode("ascii").split(":", 1)
        name = name.lower()
        if name in headers:
            raise ValueError("duplicate HTTP response header")
        headers[name] = value.strip()
    return status, headers


def request(method, target, host, token):
    return (f"{method} {target} HTTP/1.1\r\nHost: {host}\r\n"
            f"Proxy-Authorization: Bearer {token}\r\n\r\n").encode("ascii")


def download(args, token):
    started = time.perf_counter()
    digest = hashlib.sha256()
    # Fixed pattern verification buffers; memory does not grow with transfer size.
    pattern = bytes(range(251)) * 263
    with socket.create_connection(("127.0.0.1", args.port), timeout=10) as sock:
        sock.settimeout(35)
        sock.sendall(request("GET", f"/diagnostic/download?bytes={args.bytes}", "localhost", token))
        with sock.makefile("rb") as stream:
            status, headers = read_header(stream)
            if not status.startswith("HTTP/1.1 200 "):
                raise ValueError(status)
            if headers.get("content-length") != str(args.bytes):
                raise ValueError("incorrect content length")
            body_started = time.perf_counter()
            received = 0
            while received < args.bytes:
                chunk = stream.read(min(65536, args.bytes - received))
                if not chunk:
                    raise ValueError(f"truncated body at {received}")
                if chunk != pattern[received % 251:received % 251 + len(chunk)]:
                    raise ValueError(f"payload mismatch at offset {received}")
                digest.update(chunk)
                received += len(chunk)
            if stream.read(1):
                raise ValueError("unexpected trailing bytes")
    elapsed = time.perf_counter() - body_started
    print(json.dumps({"kind": "local-phone-to-host-diagnostic-NOT-cellular",
                      "bytes": received, "body_seconds": elapsed,
                      "verified_payload_mbps": received * 8 / elapsed / 1e6,
                      "total_seconds": time.perf_counter() - started,
                      "sha256": digest.hexdigest(),
                      "note": "Includes Python verification overhead; no USB link-rate claim."}))


def connect(args, token):
    url = urlsplit(args.url)
    if url.scheme not in ("http", "https") or not url.hostname or url.username or url.password:
        raise ValueError("only http/https URLs without user info")
    secure = url.scheme == "https"
    port = 443 if secure else 80
    if url.port not in (None, port):
        raise ValueError("only HTTP :80 or HTTPS :443")
    hostname = url.hostname.encode("idna").decode("ascii")
    authority = (f"[{hostname}]" if ":" in hostname else hostname) + f":{port}"
    path = url.path or "/"
    if url.query:
        path += "?" + url.query
    if any(ord(c) < 33 or ord(c) > 126 for c in path):
        raise ValueError("URL must use ASCII percent-encoding without control characters")
    with socket.create_connection(("127.0.0.1", args.port), timeout=10) as sock:
        sock.settimeout(35)
        sock.sendall(request("CONNECT", authority, authority, token))
        # Unbuffered: no TLS bytes can be swallowed by the HTTP header reader.
        with sock.makefile("rb", buffering=0) as stream:
            status, _ = read_header(stream)
        if status != "HTTP/1.1 200 Connection Established":
            raise ValueError(status)
        channel = ssl.create_default_context().wrap_socket(sock, server_hostname=hostname) if secure else sock
        with channel:
            channel.sendall((f"GET {path} HTTP/1.1\r\nHost: {authority}\r\nConnection: close\r\n\r\n").encode("ascii"))
            with channel.makefile("rb") as response:
                status, _ = read_header(response)
                # Smoke only; capped consumption, not an Internet speed benchmark.
                prefix = response.read(4096)
            print(json.dumps({"kind": "CONNECT-smoke", "tls": channel.version() if secure else None,
                              "http_status": status, "sample_bytes": len(prefix),
                              "note": ("End-to-end certificate validation enabled" if secure else "Plaintext HTTP :80") + "; not a speed result."}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8283)
    commands = parser.add_subparsers(dest="command", required=True)
    diagnostic = commands.add_parser("download", help="local phone→host bytes, NOT cellular")
    diagnostic.add_argument("--bytes", type=int, default=32 * 1024 * 1024)
    smoke = commands.add_parser("connect", help="HTTP :80 / TLS-validated HTTPS :443 GET through CONNECT")
    smoke.add_argument("--url", default="https://example.com/")
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("invalid TCP port")
    if args.command == "download" and not 1 <= args.bytes <= MAX_BYTES:
        parser.error("bytes must be in 1...268435456")
    token = os.environ.get("TF_TOKEN") or getpass.getpass("Session token (not echoed): ")
    if not 32 <= len(token) <= 128 or any(not 33 <= ord(c) <= 126 for c in token):
        parser.error("invalid session token")
    try:
        (download if args.command == "download" else connect)(args, token)
    except (OSError, ValueError) as error:
        parser.exit(1, f"FAIL: {error}\n")


if __name__ == "__main__":
    main()
