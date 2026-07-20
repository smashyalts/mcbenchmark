#!/usr/bin/env python3
"""Minimal Source RCON client, for driving a detached benchmark server's console.

The benchmark server runs without an attached stdin, so console commands (spark
profiling, /tps) need another route in. This speaks the Source RCON protocol
Minecraft implements: length-prefixed little-endian packets with a type field.

    python host/rcon.py "spark health" ["spark tps" ...]

Env: RCON_HOST (127.0.0.1), RCON_PORT (25585), RCON_PASS (benchrcon).
"""
import os
import socket
import struct
import sys

SERVERDATA_AUTH = 3
SERVERDATA_EXECCOMMAND = 2


def _pack(req_id, kind, body):
    payload = struct.pack("<ii", req_id, kind) + body.encode("utf-8") + b"\x00\x00"
    return struct.pack("<i", len(payload)) + payload


def _read_exactly(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("connection closed mid-packet")
        buf += chunk
    return buf


def _read_packet(sock):
    (length,) = struct.unpack("<i", _read_exactly(sock, 4))
    payload = _read_exactly(sock, length)
    req_id, kind = struct.unpack("<ii", payload[:8])
    return req_id, kind, payload[8:-2].decode("utf-8", "replace")


def run(commands, host, port, password, timeout=120.0):
    with socket.create_connection((host, port), timeout=10) as sock:
        sock.settimeout(timeout)
        sock.sendall(_pack(1, SERVERDATA_AUTH, password))
        req_id, _, _ = _read_packet(sock)
        if req_id == -1:
            raise SystemExit("rcon auth failed")
        for i, cmd in enumerate(commands, start=2):
            sock.sendall(_pack(i, SERVERDATA_EXECCOMMAND, cmd))
            # Long-running commands (profiler stop) can exceed one packet; read
            # until the server stops sending for this request id.
            _, _, body = _read_packet(sock)
            print(f"$ {cmd}\n{body}\n")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    run(
        sys.argv[1:],
        os.environ.get("RCON_HOST", "127.0.0.1"),
        int(os.environ.get("RCON_PORT", "25585")),
        os.environ.get("RCON_PASS", "benchrcon"),
    )
