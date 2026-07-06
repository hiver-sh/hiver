from __future__ import annotations

import asyncio
import json
import os
import re
from typing import AsyncGenerator, Optional

import httpx

from .sandbox import Sandbox, SandboxError, _to_error
from .schemas import SandboxConfig, SandboxRef
from .sse import parse_sse

# Gateway URL used when none is passed and HIVER_GATEWAY_URL is unset.
DEFAULT_GATEWAY_URL = "http://localhost:10000"
# Env var that overrides DEFAULT_GATEWAY_URL when no explicit URL is given.
GATEWAY_URL_ENV = "HIVER_GATEWAY_URL"
DEFAULT_IMAGE_NAME = "agent-base"


def resolve_gateway_url(gateway_url: Optional[str] = None) -> str:
    """Resolve the gateway base URL.

    An explicit *gateway_url* always wins; otherwise the ``HIVER_GATEWAY_URL``
    env var is used, falling back to :data:`DEFAULT_GATEWAY_URL`.
    """
    return gateway_url or os.environ.get(GATEWAY_URL_ENV) or DEFAULT_GATEWAY_URL

_SANDBOX_KEY_PATTERN = re.compile(r"^[A-Za-z0-9_-]{1,64}$")

_DEFAULT_TIMEOUT_S = 60.0


async def get_or_create_sandbox(
    key: str,
    config: SandboxConfig = SandboxConfig(),
    gateway_url: Optional[str] = None,
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
    base = resolve_gateway_url(gateway_url).rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient()
    req_timeout = timeout_s if timeout_s > 0 else None

    try:
        try:
            res = await http.post(
                f"{base}/v1/sandboxes/{key}",
                json=validated.model_dump(exclude_none=True),
                headers={
                    "x-hiver-image": validated.image or DEFAULT_IMAGE_NAME,
                    # The gateway consistent-hashes the create onto a pack host
                    # by this header (the image clusters' MAGLEV hash_policy), so
                    # every get-or-create for a key lands on the same pod. The
                    # key is also in the path, but Envoy hashes on the header.
                    "x-hiver-key": key,
                },
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
    gateway_url: Optional[str] = None,
    client: Optional[httpx.AsyncClient] = None,
    timeout_s: float = _DEFAULT_TIMEOUT_S,
) -> list[Sandbox]:
    """Return all currently running sandboxes."""
    base = resolve_gateway_url(gateway_url).rstrip("/")
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


async def watch_sandbox_events(
    gateway_url: Optional[str] = None,
    client: Optional[httpx.AsyncClient] = None,
    abort: Optional[asyncio.Event] = None,
) -> AsyncGenerator[dict, None]:
    """Watch lifecycle changes across all sandboxes as they happen.

    Yields dicts of the form
    ``{"id": "<uuid>", "key": "<key>", "status": "start|stop|die|destroy"}``
    until *abort* is set or the stream ends.
    """
    base = resolve_gateway_url(gateway_url).rstrip("/")
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
