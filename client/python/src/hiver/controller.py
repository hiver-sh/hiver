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

_SANDBOX_KEY_PATTERN = re.compile(r"^[A-Za-z0-9_-]{1,64}$")

_DEFAULT_TIMEOUT_S = 60.0


async def get_or_create_sandbox(
    key: str,
    config: SandboxConfig = SandboxConfig(),
    gateway_url: str = DEFAULT_GATEWAY_URL,
    client: Optional[httpx.AsyncClient] = None,
    timeout_s: float = _DEFAULT_TIMEOUT_S,
) -> Sandbox:
    """
    Create a sandbox, or fetch the existing one when `key` is already in use.
    The key acts as an idempotency key: calling again with the same key returns
    the same sandbox and leaves the supplied `config` unapplied. Resolves once
    the sandbox is ready to accept requests.
    """
    if not _SANDBOX_KEY_PATTERN.match(key):
        raise ValueError(
            f"get_or_create_sandbox: key {key!r} must match {_SANDBOX_KEY_PATTERN.pattern}"
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
                f"{base}/controller/v1/sandboxes/{key}",
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
        return Sandbox(ref, base, client=http if not owns_client else None)
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
    """Permanently stop and remove a sandbox."""
    base = gateway_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()
    req_timeout = timeout_s if timeout_s > 0 else None
    url = f"{base}/controller/v1/shutdown/{sandbox.key}"
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
    """Watch lifecycle changes across all sandboxes as they happen.

    Yields dicts of the form
    ``{"id": "<uuid>", "key": "<key>", "status": "start|stop|die|destroy"}``
    until *abort* is set or the stream ends.
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


def _is_connection_refused(err: httpx.ConnectError) -> bool:
    msg = str(err).lower()
    return "connection refused" in msg or "econnrefused" in msg
