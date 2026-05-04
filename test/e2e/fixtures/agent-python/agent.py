import contextlib
import time
import urllib.request
import urllib.error


def http_get(url):
    with contextlib.suppress(urllib.error.HTTPError, OSError):
        with urllib.request.urlopen(url, timeout=5):
            pass


def http_post(url):
    with contextlib.suppress(urllib.error.HTTPError, OSError):
        req = urllib.request.Request(url, data=b"", method="POST")
        with urllib.request.urlopen(req, timeout=5):
            pass


def fs_write(path, content):
    with contextlib.suppress(OSError):
        with open(path, "w") as f:
            f.write(content)


def fs_read(path):
    with contextlib.suppress(OSError):
        with open(path) as f:
            f.read()


# Probes are written against the egress rules in spec.yaml: the proxy
# allows GET / on upstream-allowed (with a header override) and TLS to
# go.dev (host-only match via SNI). Everything else is denied.
http_get("http://upstream-allowed:17080/")            # rule match
http_post("http://upstream-allowed:17080/")           # method denied
http_get("http://upstream-allowed:17080/forbidden")   # path denied
http_get("http://upstream-denied:17081/")             # host denied
http_get("https://go.dev/solutions/case-studies/")    # TLS intercepted, path allowed
http_get("https://go.dev/doc/devel/release")                           # TLS intercepted, path denied

# /workspace is a FUSE mount — sbxfuse mediates per-op via ACLs.
fs_write("/workspace/hello.txt", "hello from python")
fs_read("/workspace/hello.txt")
fs_read("/workspace/secret/keys.txt")                 # ACL deny → ENOENT

# Block forever so the sandbox-pod stays up for inspection
# (docker exec into it, peek at /workspace, attach a debugger, etc.).
while True:
    time.sleep(3600)
