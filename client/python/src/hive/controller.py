from __future__ import annotations

import asyncio
import json
import re
from typing import Optional

import httpx

from .sandbox import Sandbox, SandboxError, _FETCH_TIMEOUT, _to_error
from .schemas import SandboxConfig, SandboxDetail, SandboxRef

DEFAULT_CONTROLLER_URL = "http://localhost:9000"

_SANDBOX_ID_PATTERN = re.compile(r"^[A-Za-z0-9_-]{1,64}$")

_DEFAULT_READINESS_TIMEOUT_S = 30.0
_READINESS_POLL_INTERVAL_S = 0.2


async def get_or_create_sandbox(
    id: str,
    config: SandboxConfig,
    controller_url: str = DEFAULT_CONTROLLER_URL,
    client: Optional[httpx.AsyncClient] = None,
    readiness_timeout_s: float = _DEFAULT_READINESS_TIMEOUT_S,
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
    validated = SandboxConfig.model_validate(config.model_dump(exclude_none=True))
    base = controller_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient(timeout=_FETCH_TIMEOUT)

    try:
        try:
            res = await http.put(
                f"{base}/v1/sandboxes/{id}",
                json=validated.model_dump(exclude_none=True),
            )
        except httpx.ConnectError as err:
            if _is_connection_refused(err):
                raise SandboxError(
                    "get_or_create_sandbox",
                    0,
                    f"controller is not reachable at {base} (connection refused). Is it running?",
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
        if readiness_timeout_s > 0:
            await _wait_until_reachable(sandbox, readiness_timeout_s)
        return sandbox
    except Exception:
        if owns_client:
            await http.aclose()
        raise


async def get_sandbox(
    id: str,
    controller_url: str = DEFAULT_CONTROLLER_URL,
    client: Optional[httpx.AsyncClient] = None,
) -> SandboxDetail:
    """
    Fetch the detail record for a single sandbox, including the terminal attach command.
    Raises SandboxError with status 404 if the sandbox does not exist.
    """
    base = controller_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient(timeout=_FETCH_TIMEOUT)

    try:
        try:
            res = await http.get(f"{base}/v1/sandboxes/{id}")
        except httpx.ConnectError as err:
            if _is_connection_refused(err):
                raise SandboxError(
                    "get_sandbox",
                    0,
                    f"controller is not reachable at {base} (connection refused). Is it running?",
                ) from err
            raise

        if res.status_code != 200:
            raise _to_error(res, "get_sandbox")

        return SandboxDetail.model_validate(res.json())
    except Exception:
        if owns_client:
            await http.aclose()
        raise


async def list_sandboxes(
    controller_url: str = DEFAULT_CONTROLLER_URL,
    client: Optional[httpx.AsyncClient] = None,
) -> list[Sandbox]:
    """Return all currently running sandboxes."""
    base = controller_url.rstrip("/")
    owns_client = client is None
    http = client or httpx.AsyncClient(timeout=_FETCH_TIMEOUT)

    try:
        try:
            res = await http.get(f"{base}/v1/sandboxes")
        except httpx.ConnectError as err:
            if _is_connection_refused(err):
                raise SandboxError(
                    "list_sandboxes",
                    0,
                    f"controller is not reachable at {base} (connection refused). Is it running?",
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


async def shutdown(sandbox: Sandbox) -> None:
    """Stop the sandbox container and remove it."""
    url = f"{sandbox.controller_url}/v1/shutdown/{sandbox.id}"
    try:
        res = await sandbox._client.post(url)
    except httpx.ConnectError as err:
        if _is_connection_refused(err):
            raise SandboxError(
                "shutdown",
                0,
                f"controller is not reachable at {sandbox.controller_url} (connection refused). Is it running?",
            ) from err
        raise

    if res.status_code == 204:
        return

    raise _to_error(res, "shutdown")


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
