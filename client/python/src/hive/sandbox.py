from __future__ import annotations

import asyncio
import json
from typing import AsyncGenerator, Optional, Union

import httpx

from .schemas import (
    ApplyResult,
    SandboxConfig,
    SandboxEvent,
    SandboxEventAdapter,
    SandboxRef,
)
from .sse import parse_sse

_FETCH_TIMEOUT = httpx.Timeout(connect=3.0, read=3.0, write=3.0, pool=3.0)


class SandboxError(Exception):
    def __init__(
        self,
        operation: str,
        status: int,
        message: str,
        body: Optional[dict] = None,
    ) -> None:
        super().__init__(f"{operation}: {message}")
        self.status = status
        self.operation = operation
        self.body = body


def _to_error(res: httpx.Response, operation: str) -> SandboxError:
    text = res.text
    body: Optional[dict] = None
    try:
        parsed = json.loads(text)
        if isinstance(parsed, dict) and "error" in parsed:
            body = parsed
    except Exception:
        pass
    message = (body or {}).get("error") or text or str(res.status_code)
    return SandboxError(operation, res.status_code, message, body)


class Sandbox:
    """
    Handle to a provisioned sandbox. Returned by `get_or_create_sandbox`;
    not constructed directly by callers.
    """

    def __init__(
        self,
        ref: SandboxRef,
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self.id = ref.id
        self.api_server_url = ref.endpoint.rstrip("/")
        self.exposed_endpoint: Optional[str] = ref.exposed_endpoint
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=_FETCH_TIMEOUT)

    async def aclose(self) -> None:
        if self._owns_client:
            await self._client.aclose()

    async def __aenter__(self) -> "Sandbox":
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.aclose()

    async def ping(self) -> None:
        """Reset the sandbox's TTL countdown."""
        res = await self._client.get(f"{self.api_server_url}/v1/ping")
        if not res.is_success:
            raise _to_error(res, "ping")

    async def get_config(self) -> SandboxConfig:
        """Read the current SandboxConfig."""
        res = await self._client.get(f"{self.api_server_url}/v1/config")
        if not res.is_success:
            raise _to_error(res, "get_config")
        return SandboxConfig.model_validate(res.json())

    async def apply_config(self, config: SandboxConfig) -> ApplyResult:
        """
        Apply a desired SandboxConfig. Returns an ApplyResult whose
        `applied` field reports whether the change was committed or
        rolled back.
        """
        validated = SandboxConfig.model_validate(config.model_dump(exclude_none=True))
        res = await self._client.put(
            f"{self.api_server_url}/v1/config",
            json=validated.model_dump(exclude_none=True),
        )
        if not res.is_success:
            raise _to_error(res, "apply_config")
        return ApplyResult.model_validate(res.json())

    async def exec(
        self,
        command: str,
        cwd: Optional[str] = None,
    ) -> dict[str, object]:
        """
        Run `command` inside the sandbox and return buffered stdout, stderr,
        and exit_code once the process finishes.
        """
        body: dict[str, str] = {"command": command}
        if cwd is not None:
            body["cwd"] = cwd
        res = await self._client.post(
            f"{self.api_server_url}/v1/exec",
            json=body,
        )
        if not res.is_success:
            raise _to_error(res, "exec")
        return res.json()

    async def exec_stream(
        self,
        command: str,
        cwd: Optional[str] = None,
    ) -> AsyncGenerator[dict[str, object], None]:
        """
        Run `command` inside the sandbox and stream output as an async
        generator of dicts. Each dict has a `type` key of "stdout",
        "stderr", or "exit". The final dict is the "exit" event.
        """
        body: dict[str, str] = {"command": command}
        if cwd is not None:
            body["cwd"] = cwd
        sse_timeout = httpx.Timeout(None, connect=_FETCH_TIMEOUT.connect)
        async with self._client.stream(
            "POST",
            f"{self.api_server_url}/v1/exec-stream",
            json=body,
            headers={"accept": "text/event-stream"},
            timeout=sse_timeout,
        ) as res:
            if not res.is_success:
                await res.aread()
                raise _to_error(res, "exec_stream")
            async for frame in parse_sse(res, None):
                yield json.loads(frame.data)

    async def list_directory(self, path: str) -> list[dict[str, object]]:
        """
        List the immediate children of a directory under a sandbox mount.
        `path` is the agent-visible absolute path (e.g. `/workspace`).
        Returns a list of entries with `name`, `path`, `is_dir`, and `size`.
        """
        res = await self._client.get(
            f"{self.api_server_url}/v1/directories",
            params={"path": path},
        )
        if not res.is_success:
            raise _to_error(res, "list_directory")
        return res.json()["entries"]

    async def download_file(self, path: str) -> bytes:
        """
        Download a file from a sandbox mount. `path` is the agent-visible
        absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
        """
        res = await self._client.get(
            f"{self.api_server_url}/v1/file",
            params={"path": path},
        )
        if not res.is_success:
            raise _to_error(res, "download_file")
        return res.content

    async def upload_file(
        self,
        destination: str,
        filename: str,
        content: Union[bytes, str],
    ) -> dict[str, object]:
        """
        Upload `content` as a file to `destination` (which must equal one
        of the configured `fs[].mount` paths). `filename` becomes the
        basename written under `destination`. Returns the agent-visible
        path and byte count the server reports.
        """
        if isinstance(content, str):
            content = content.encode("utf-8")
        res = await self._client.post(
            f"{self.api_server_url}/v1/file",
            data={"destination": destination},
            files={"file": (filename, content)},
        )
        if not res.is_success:
            raise _to_error(res, "upload_file")
        return res.json()

    async def get_events_stream(
        self,
        last_event_id: Optional[int] = None,
        abort: Optional[asyncio.Event] = None,
        max_retries: int = 3,
    ) -> AsyncGenerator[SandboxEvent, None]:
        """
        Long-lived async generator over SandboxEvents.

        Auto-resumes across disconnects: if the underlying SSE connection
        drops the generator silently reopens it with the last id observed,
        so the consumer never sees a gap. Reconnect uses exponential
        backoff up to 30s. Terminate the stream by setting `abort` or
        cancelling the calling task.
        """
        backoff_s = 0.2
        retry = 0

        while abort is None or not abort.is_set():
            if retry > max_retries:
                return
            try:
                async for event in self._open_events_stream(last_event_id, abort):
                    last_event_id = event.id  # type: ignore[union-attr]
                    backoff_s = 0.2
                    yield event
            except asyncio.CancelledError:
                return
            except Exception:
                if abort is not None and abort.is_set():
                    return

            await _sleep(backoff_s, abort)
            backoff_s = min(backoff_s * 2, 30.0)
            retry += 1

    async def _open_events_stream(
        self,
        last_event_id: Optional[int],
        abort: Optional[asyncio.Event],
    ) -> AsyncGenerator[SandboxEvent, None]:
        params: dict[str, str] = {}
        if last_event_id is not None:
            params["lastEventId"] = str(last_event_id)

        sse_timeout = httpx.Timeout(None, connect=_FETCH_TIMEOUT.connect)
        async with self._client.stream(
            "GET",
            f"{self.api_server_url}/v1/events",
            headers={"accept": "text/event-stream"},
            params=params,
            timeout=sse_timeout,
        ) as res:
            if not res.is_success:
                await res.aread()
                raise _to_error(res, "events")
            async for frame in parse_sse(res, abort):
                yield SandboxEventAdapter.validate_python(json.loads(frame.data))


async def _sleep(seconds: float, abort: Optional[asyncio.Event]) -> None:
    """Sleep for `seconds`, waking early if `abort` is set."""
    if abort is None:
        await asyncio.sleep(seconds)
        return
    try:
        await asyncio.wait_for(abort.wait(), timeout=seconds)
    except asyncio.TimeoutError:
        pass
