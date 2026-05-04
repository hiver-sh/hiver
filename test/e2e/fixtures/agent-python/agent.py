# Python agent for the sandbox-pod E2E test.
#
# This script intentionally produces no application-level logging and
# does NOT cooperate with any proxy environment: it makes plain HTTP
# requests as if there were no sandbox in front of it. iptables in the
# sandbox-pod's netns transparently REDIRECTs the agent's outbound TCP
# to sbxproxy, which decides allow/deny by sniffing the request and
# matching the Host header against the egress allowlist. The operation
# log lives entirely in sandboxd, which tails the proxy + FUSE audit
# streams (DESIGN.md §9.1) — the agent's job is just to exercise the
# boundaries.
#
# Required env vars (sandboxd exports WORKSPACE; the host test threads
# ALLOW_URL / DENY_URL / DENY_PATH through agent.env):
#   WORKSPACE         - /workspace
#   ALLOW_URL         - URL the proxy should allow (host: upstream-allowed)
#   DENY_URL          - URL the proxy should deny  (host: upstream-denied)
#   DENY_PATH         - path inside the workspace that the FUSE ACL denies
import contextlib
import os
import time
import urllib.request
import urllib.error


def http_get(url):
    if not url:
        return
    with contextlib.suppress(urllib.error.HTTPError, OSError):
        with urllib.request.urlopen(url, timeout=5):
            pass


def fs_write(path, content):
    with contextlib.suppress(OSError):
        with open(path, "w") as f:
            f.write(content)


def fs_read(path):
    with contextlib.suppress(OSError):
        with open(path) as f:
            f.read()


http_get(os.environ.get("ALLOW_URL", ""))
http_get(os.environ.get("DENY_URL", ""))

ws = os.environ.get("WORKSPACE", "")
if ws:
    fs_write(os.path.join(ws, "hello.txt"), "hello from python")
    fs_read(os.path.join(ws, "hello.txt"))

deny_path = os.environ.get("DENY_PATH", "")
if deny_path:
    fs_read(deny_path)

# Block forever so the sandbox-pod stays up for inspection
# (docker exec into it, peek at /workspace, attach a debugger, etc.).
while True:
    time.sleep(3600)
