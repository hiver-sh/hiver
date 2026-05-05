# Minimal gdrive-backed agent fixture.
#
# Two probes only — write a file then read it back. /workspace is a
# FUSE mount whose backend is the journaled gdrive cache: writes hit
# the local buffer, sbxfuse enqueues an Oplog Put, the uploader
# goroutine pushes it to Drive. Reads return from the buffer. The
# host-side test asserts both the agent's intent lines and sandboxd's
# observation of the FUSE mutation.
import contextlib
import json
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

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


_ingress_server = HTTPServer(("0.0.0.0", 18000), _IngressHandler)
threading.Thread(target=_ingress_server.serve_forever, daemon=True).start()


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


fs_write("/workspace/note.txt", "hello from gdrive-backed workspace")
fs_read("/workspace/note.txt")

print("DONE", flush=True)

while True:
    time.sleep(3600)
