from __future__ import annotations

import asyncio
import json
import re
from typing import AsyncGenerator, Optional

import httpx

from .sandbox import Sandbox, SandboxError, _to_error
from .schemas import SandboxConfig, SandboxRef
from .sse import parse_sse

DEFAULT_GATEWAY_URL = "http://localhost:10000"

_SANDBOX_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{1,64}$")

_DEFAULT_TIMEOUT_S = 30.0
_READINESS_POLL_INTERVAL_S = 0.2


async def get_or_create_sandbox(
    id: str,
    config: SandboxConfig = SandboxConfig(),
    gateway_url: str = DEFAULT_GATEWAY_URL,
    client: Optional[httpx.AsyncClient] = None,
    timeout_s: float = _DEFAULT_TIMEOUT_S,
) -> Sandbox:
    """
    Idempotent provision against PUT /v1/sandboxes/{id}. If a sandbox
    with `id` already exists the controller returns it unchanged and
    the supplied `config` is ignored; otherwise the controller creates
    a new sandbox from `config`.

    `config` is validated before the request is sent — a bad config
    fails fast on the caller side instead of producing a 400 from the
    controller.
    """
    if not _SANDBOX_ID_PATTERN.match(id):
        raise ValueError(
            f"get_or_create_sandbox: id {id!r} must match {_SANDBOX_ID_PATTERN.pattern}"
        )
    data = {
        "fs": [
            {
                "backend": "local",
                "mount": "/workspace",
                "acls": [{"path": "/workspace/**", "access": "rw"}],
            }
        ],
        **config.model_dump(exclude_none=True),
    }
    validated = SandboxConfig.model_validate(data)
    base = gateway_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()
    req_timeout = timeout_s if timeout_s > 0 else None

    try:
        try:
            res = await http.put(
                f"{base}/controller/v1/sandboxes/{id}",
                json=validated.model_dump(exclude_none=True),
                timeout=req_timeout,
            )
        except httpx.ConnectError as err:
            if _is_connection_refused(err):
                raise SandboxError(
                    "get_or_create_sandbox",
                    0,
                    f"gateway is not reachable at {base} (connection refused). Is it running?",
                ) from err
            raise

        if res.status_code not in (200, 201):
            text = res.text
            body: Optional[dict] = None
            try:
                parsed = json.loads(text)
                if isinstance(parsed, dict) and "error" in parsed:
                    body = parsed
            except Exception:
                pass
            raise SandboxError(
                "get_or_create_sandbox",
                res.status_code,
                (body or {}).get("error") or text or str(res.status_code),
                body,
            )

        ref = SandboxRef.model_validate(res.json())
        sandbox = Sandbox(ref, base, client=http if not owns_client else None)
        if timeout_s > 0:
            await _wait_until_reachable(sandbox, timeout_s)
        return sandbox
    except Exception:
        if owns_client:
            await http.aclose()
        raise


async def list_sandboxes(
    gateway_url: str = DEFAULT_GATEWAY_URL,
    client: Optional[httpx.AsyncClient] = None,
    timeout_s: float = _DEFAULT_TIMEOUT_S,
) -> list[Sandbox]:
    """Return all currently running sandboxes."""
    base = gateway_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()
    req_timeout = timeout_s if timeout_s > 0 else None

    try:
        try:
            res = await http.get(f"{base}/controller/v1/sandboxes", timeout=req_timeout)
        except httpx.ConnectError as err:
            if _is_connection_refused(err):
                raise SandboxError(
                    "list_sandboxes",
                    0,
                    f"gateway is not reachable at {base} (connection refused). Is it running?",
                ) from err
            raise

        if res.status_code != 200:
            raise _to_error(res, "list_sandboxes")

        refs = [SandboxRef.model_validate(r) for r in res.json()]
        return [Sandbox(ref, base, client=http if not owns_client else None) for ref in refs]
    except Exception:
        if owns_client:
            await http.aclose()
        raise


async def shutdown(
    sandbox: Sandbox,
    gateway_url: str = DEFAULT_GATEWAY_URL,
    client: Optional[httpx.AsyncClient] = None,
    timeout_s: float = _DEFAULT_TIMEOUT_S,
) -> None:
    """Stop the sandbox container and remove it."""
    base = gateway_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()
    req_timeout = timeout_s if timeout_s > 0 else None
    url = f"{base}/controller/v1/shutdown/{sandbox.id}"
    try:
        res = await http.post(url, timeout=req_timeout)
    except httpx.ConnectError as err:
        if _is_connection_refused(err):
            raise SandboxError(
                "shutdown",
                0,
                f"gateway is not reachable at {base} (connection refused). Is it running?",
            ) from err
        raise
    finally:
        if owns_client:
            await http.aclose()

    if res.status_code == 204:
        return

    raise _to_error(res, "shutdown")


async def watch_sandbox_events(
    gateway_url: str = DEFAULT_GATEWAY_URL,
    client: Optional[httpx.AsyncClient] = None,
    abort: Optional[asyncio.Event] = None,
) -> AsyncGenerator[dict, None]:
    """Stream sandbox lifecycle events via SSE from GET /v1/sandboxes/events.

    Yields dicts of the form ``{"id": "<sandbox-id>", "status": "start|stop|die|destroy"}``
    until *abort* is set or the server closes the stream.
    """
    base = gateway_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()

    try:
        async with http.stream(
            "GET",
            f"{base}/controller/v1/sandboxes/events",
            headers={"Accept": "text/event-stream"},
        ) as res:
            if res.status_code != 200:
                raise SandboxError(
                    "watch_sandbox_events",
                    res.status_code,
                    (await res.aread()).decode(),
                )
            async for frame in parse_sse(res, abort):
                try:
                    yield json.loads(frame.data)
                except Exception:
                    pass
    except httpx.ConnectError as err:
        if _is_connection_refused(err):
            raise SandboxError(
                "watch_sandbox_events",
                0,
                f"gateway is not reachable at {base} (connection refused). Is it running?",
            ) from err
        raise
    finally:
        if owns_client:
            await http.aclose()


async def _wait_until_reachable(sandbox: Sandbox, timeout_s: float) -> None:
    loop = asyncio.get_event_loop()
    deadline = loop.time() + timeout_s
    last_err: Optional[Exception] = None

    while loop.time() < deadline:
        try:
            await sandbox.ping()
            return
        except Exception as err:
            last_err = err
        await asyncio.sleep(_READINESS_POLL_INTERVAL_S)

    detail = f": {last_err}" if last_err else ""
    raise SandboxError(
        "get_or_create_sandbox",
        0,
        f"sandbox {sandbox.id} did not become reachable at {sandbox.api_server_url} "
        f"within {timeout_s:.0f}s{detail}",
    )


def _is_connection_refused(err: httpx.ConnectError) -> bool:
    msg = str(err).lower()
    return "connection refused" in msg or "econnrefused" in msg
