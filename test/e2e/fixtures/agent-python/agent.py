import contextlib
import json
import subprocess
import threading
import time
import urllib.request
import urllib.error
from http.server import BaseHTTPRequestHandler, HTTPServer


# Inbound HTTP listener — the host can POST to the sandbox-pod once the
# agent has emitted "DONE", to verify host→agent ingress works. The
# sandbox-pod publishes container:18000 to its own host port. Routes:
#
#   POST /hello   echo probe; surfaces as
#                 "[agent:out] INGRESS POST /hello <body!r>".
#
#   POST /exec    body is a bash command; the agent runs it under
#                 /bin/bash -c and returns
#                 {"exit_code": int, "stdout": str, "stderr": str}.
#                 Stdout-prints "[agent:out] INGRESS EXEC <cmd!r>
#                 → exit=<code>" so the host-side test can assert.
class _IngressHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length).decode("utf-8", errors="replace")

        if self.path == "/exec":
            self._handle_exec(body)
            return

        print(f"INGRESS POST {self.path} {body!r}", flush=True)
        self.send_response(200)
        self.send_header("Content-Length", "3")
        self.end_headers()
        self.wfile.write(b"ok\n")

    def _handle_exec(self, command: str) -> None:
        try:
            result = subprocess.run(
                ["/bin/bash", "-c", command],
                capture_output=True,
                text=True,
                timeout=30,
            )
            payload = {
                "exit_code": result.returncode,
                "stdout": result.stdout,
                "stderr": result.stderr,
            }
        except subprocess.TimeoutExpired as e:
            payload = {
                "exit_code": -1,
                "stdout": e.stdout or "",
                "stderr": (e.stderr or "") + "\n[agent: command timed out]",
            }
        except Exception as e:
            payload = {"exit_code": -1, "stdout": "", "stderr": str(e)}

        print(
            f"INGRESS EXEC {command!r} → exit={payload['exit_code']}",
            flush=True,
        )
        body_bytes = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body_bytes)))
        self.end_headers()
        self.wfile.write(body_bytes)

    def log_message(self, *_args, **_kwargs):
        pass


def _serve_ingress():
    HTTPServer(("0.0.0.0", 18000), _IngressHandler).serve_forever()


threading.Thread(target=_serve_ingress, daemon=True).start()


def http_get(url):
    print(f"GET {url}", flush=True)
    with contextlib.suppress(urllib.error.HTTPError, OSError):
        with urllib.request.urlopen(url, timeout=5):
            pass


def http_post(url):
    print(f"POST {url}", flush=True)
    with contextlib.suppress(urllib.error.HTTPError, OSError):
        req = urllib.request.Request(url, data=b"", method="POST")
        with urllib.request.urlopen(req, timeout=5):
            pass


def fs_write(path, content):
    print(f"WRITE {path}", flush=True)
    with contextlib.suppress(OSError):
        with open(path, "w") as f:
            f.write(content)


def fs_read(path):
    print(f"READ {path}", flush=True)
    with contextlib.suppress(OSError):
        with open(path) as f:
            f.read()


# Probes are written against the egress rules in spec.yaml: the proxy
# allows GET / on upstream-allowed (with a header override) and TLS to
# go.dev/solutions/case-studies (intercepted, path-matched). Everything
# else is denied.
http_get("http://upstream-allowed:17080/")            # rule match
http_post("http://upstream-allowed:17080/")           # method denied
http_get("http://upstream-allowed:17080/forbidden")   # path denied
http_get("http://upstream-denied:17081/")             # host denied
http_get("https://go.dev/solutions/case-studies/")    # TLS intercepted, path allowed
http_get("https://go.dev/doc/devel/release")          # TLS intercepted, path denied

# /workspace is a FUSE mount — sbxfuse mediates per-op via ACLs.
fs_write("/workspace/hello.txt", "hello from python")
fs_read("/workspace/hello.txt")
fs_read("/workspace/secret/keys.txt")                 # ACL deny → ENOENT

print("DONE", flush=True)

# Block forever so the sandbox-pod stays up for inspection
# (docker exec into it, peek at /workspace, attach a debugger, etc.).
while True:
    time.sleep(3600)
