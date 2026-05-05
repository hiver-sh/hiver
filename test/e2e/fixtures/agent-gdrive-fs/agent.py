# Minimal gdrive-backed agent fixture.
#
# Two probes only — write a file then read it back. /workspace is a
# FUSE mount whose backend is the journaled gdrive cache: writes hit
# the local buffer, sbxfuse enqueues an Oplog Put, the uploader
# goroutine pushes it to Drive. Reads return from the buffer. The
# host-side test asserts both the agent's intent lines and sandboxd's
# observation of the FUSE mutation.
import contextlib
import time


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
