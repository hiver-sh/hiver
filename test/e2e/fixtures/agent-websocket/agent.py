"""WebSocket e2e probe.

Tests:
  1. ws:// to allowed upstream-ws:17082 — expects 101 + echo.
  2. ws:// to denied upstream-denied:17082 — expects 403 from proxy.

Uses only the Python standard library (raw TCP + manual frame codec).
"""

import io
import os
import socket
import struct
import time


# ── WebSocket codec ──────────────────────────────────────────────────────────

def _ws_connect(host, port, path="/"):
    """Return (socket, leftover_bytes, status_code) after the HTTP handshake."""
    s = socket.create_connection((host, port), timeout=10)
    import base64
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall((
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "\r\n"
    ).encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = s.recv(4096)
        if not chunk:
            raise EOFError("connection closed during handshake")
        buf += chunk
    status = int(buf.split(b"\r\n")[0].split()[1])
    leftover = buf[buf.index(b"\r\n\r\n") + 4:]
    return s, leftover, status


def _ws_send_text(sock, text):
    """Send a masked text frame (client→server frames must be masked)."""
    data = text.encode()
    mask = os.urandom(4)
    masked = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
    sock.sendall(bytes([0x81, 0x80 | len(data)]) + mask + masked)


def _ws_recv_text(sock, buf=b""):
    """Read one WebSocket frame; return (payload_str, remaining_buf)."""
    r = io.BytesIO(buf)
    r.seek(0, 2)  # position at end so reads extend buf

    def fill(n):
        nonlocal buf
        while len(buf) < n:
            chunk = sock.recv(4096)
            if not chunk:
                raise EOFError("connection closed")
            buf += chunk
        return buf

    fill(2)
    b0, b1 = buf[0], buf[1]
    masked = (b1 & 0x80) != 0
    pl = b1 & 0x7F
    pos = 2

    if pl == 126:
        fill(pos + 2)
        pl = struct.unpack_from(">H", buf, pos)[0]
        pos += 2
    elif pl == 127:
        fill(pos + 8)
        pl = struct.unpack_from(">Q", buf, pos)[0]
        pos += 8

    if masked:
        fill(pos + 4)
        mkey = buf[pos:pos + 4]
        pos += 4
    else:
        mkey = None

    fill(pos + pl)
    payload = bytearray(buf[pos:pos + pl])
    if mkey:
        for i in range(len(payload)):
            payload[i] ^= mkey[i % 4]

    return payload.decode(), buf[pos + pl:]


# ── Test 1: ws:// allowed → should get 101 + echo ───────────────────────────

try:
    sock, leftover, code = _ws_connect("upstream-ws", 17082)
    if code != 101:
        print(f"WS ALLOWED ERROR: expected 101 got {code}", flush=True)
    else:
        _ws_send_text(sock, "hello-ws-plain")
        msg, _ = _ws_recv_text(sock, leftover)
        if msg == "hello-ws-plain":
            print("WS ALLOWED OK", flush=True)
        else:
            print(f"WS ALLOWED MISMATCH: got {msg!r}", flush=True)
    sock.close()
except Exception as e:
    print(f"WS ALLOWED ERROR: {e}", flush=True)


# ── Test 2: ws:// denied → proxy should return 403 ──────────────────────────

try:
    sock, _, code = _ws_connect("upstream-denied", 17082)
    sock.close()
    if code == 403:
        print("WS DENIED OK", flush=True)
    else:
        print(f"WS DENIED ERROR: expected 403 got {code}", flush=True)
except Exception as e:
    print(f"WS DENIED ERROR: {e}", flush=True)


print("DONE", flush=True)
while True:
    time.sleep(3600)
