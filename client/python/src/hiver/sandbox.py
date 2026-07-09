from __future__ import annotations

import asyncio
import json
import uuid
from typing import AsyncGenerator, Callable, Coroutine, Any, AsyncIterable, Optional, Union
from urllib.parse import quote

import httpx

from .schemas import (
    ApplyResult,
    SandboxConfig,
    SandboxEvent,
    SandboxEventAdapter,
    SandboxInfo,
    SandboxRef,
    Snapshot,
    SnapshotResult,
)
from .sse import parse_sse

_FETCH_TIMEOUT = httpx.Timeout(connect=3.0, read=3.0, write=3.0, pool=3.0)


class SandboxError(Exception):
    """Raised when a client operation against a sandbox or the controller fails."""

    def __init__(
        self,
        operation: str,
        status: int,
        message: str,
        body: Optional[dict] = None,
    ) -> None:
        super().__init__(f"{operation}: {message}")
        self.status = status
        """HTTP status from the failed response, or ``0`` if the request never reached the server."""
        self.operation = operation
        """The client operation that failed (e.g. ``"apply_config"``)."""
        self.body = body
        """Structured error payload from the server, when one was returned."""


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


class ExecProcess:
    """
    Handle to an interactive command started with :meth:`Sandbox.exec_stream`.
    Stream output via :attr:`pipes`, send input via :meth:`write_stdin`, and
    await the result via :attr:`exit_code`.
    """

    id: str
    """Unique id for this exec invocation."""
    pipes: AsyncIterable[dict[str, str]]
    """Async iterable of output chunks emitted as the process runs — each is ``{"stdout": ...}`` or ``{"stderr": ...}``."""
    exit_code: "asyncio.Future[int]"
    """Resolves with the process exit code once it finishes."""

    def __init__(
        self,
        id: str,
        pipes: AsyncIterable[dict[str, str]],
        exit_code: asyncio.Future[int],
        write_stdin_fn: Callable[[str], Coroutine[Any, Any, None]],
    ) -> None:
        self.id = id
        self.pipes = pipes
        self.exit_code = exit_code
        self._write_stdin_fn = write_stdin_fn

    async def write_stdin(self, data: str) -> None:
        """Send `data` to the process's stdin."""
        await self._write_stdin_fn(data)


class Sandbox:
    """
    Handle to a provisioned sandbox. Returned by :func:`get_or_create_sandbox`;
    not constructed directly by callers.
    """

    id: str
    """Server-assigned unique identifier (uuid)."""
    key: str
    """Caller-chosen key the sandbox was provisioned under; routes requests."""
    api_server_url: str
    """Base URL of the per-sandbox API server."""

    def __init__(
        self,
        ref: SandboxRef,
        gateway_url: str,
        client: Optional[httpx.AsyncClient] = None,
    ) -> None:
        self.id = ref.id
        self.key = ref.key
        self.api_server_url = f"{gateway_url.rstrip('/')}/sandbox/{ref.id}"
        self._owns_client = client is None
        self._client = client or httpx.AsyncClient(timeout=_FETCH_TIMEOUT)

    def proxy_url(self, port: Union[int, str]) -> str:
        """
        Base proxy URL for reaching a port inside the sandbox. Ends with a
        trailing slash, so it reaches the port's root as-is; append a path to
        reach an endpoint, e.g. ``sandbox.proxy_url(8080) + "health"``.
        """
        return f"{self.api_server_url}/v1/{self.key}/proxy/{port}/"

    async def aclose(self) -> None:
        """Close the underlying HTTP client if this sandbox owns it."""
        if self._owns_client:
            await self._client.aclose()

    async def __aenter__(self) -> "Sandbox":
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.aclose()

    async def ping(self) -> None:
        """Keep the sandbox alive by resetting its TTL countdown."""
        res = await self._client.get(f"{self.api_server_url}/v1/{self.key}/ping")
        if not res.is_success:
            raise _to_error(res, "ping")

    async def shutdown(self) -> None:
        """Tear this sandbox down via ``DELETE /v1/<key>``, cancelling its lifecycle."""
        res = await self._client.delete(f"{self.api_server_url}/v1/{self.key}")
        if not res.is_success:
            raise _to_error(res, "shutdown")

    async def get_ports(self) -> list[int]:
        """
        List the network ports the sandbox exposes. Reach each one via
        :meth:`proxy_url`.
        """
        res = await self._client.get(f"{self.api_server_url}/v1/{self.key}/ports")
        if not res.is_success:
            raise _to_error(res, "get_ports")
        return res.json()

    async def get_info(self) -> SandboxInfo:
        """
        Read internal runtime info about the sandbox — currently the isolation
        mechanism in use, which is selected automatically from the image (a
        microvm image ships a guest root filesystem) rather than configured.
        """
        res = await self._client.get(f"{self.api_server_url}/v1/{self.key}/info")
        if not res.is_success:
            raise _to_error(res, "get_info")
        return SandboxInfo.model_validate(res.json())

    async def get_config(self) -> SandboxConfig:
        """Read the current SandboxConfig."""
        res = await self._client.get(f"{self.api_server_url}/v1/{self.key}/config")
        if not res.is_success:
            raise _to_error(res, "get_config")
        return SandboxConfig.model_validate(res.json())

    async def apply_config(self, config: SandboxConfig) -> ApplyResult:
        """
        Apply a new SandboxConfig. The change is all-or-nothing: the returned
        ApplyResult's ``applied`` field reports whether it was committed or
        rolled back, and ``changes`` details what was added or removed.
        """
        validated = SandboxConfig.model_validate(config.model_dump(exclude_none=True))
        res = await self._client.put(
            f"{self.api_server_url}/v1/{self.key}/config",
            json=validated.model_dump(exclude_none=True),
        )
        if not res.is_success:
            raise _to_error(res, "apply_config")
        return ApplyResult.model_validate(res.json())

    async def snapshot(self, request: Snapshot) -> SnapshotResult:
        """
        Capture a snapshot of the running sandbox now, without stopping it. The
        request selects which parts to capture: ``vm`` (full microVM state, keyed
        for a later resume; a no-op on container isolation) and/or ``files`` (the
        writable filesystem). Each part is reported independently in the result.
        """
        validated = Snapshot.model_validate(request.model_dump(exclude_none=True))
        res = await self._client.post(
            f"{self.api_server_url}/v1/{self.key}/snapshot",
            json=validated.model_dump(exclude_none=True),
        )
        if not res.is_success:
            raise _to_error(res, "snapshot")
        return SnapshotResult.model_validate(res.json())

    async def exec(
        self,
        command: Union[str, list[str]],
        cwd: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
    ) -> dict[str, object]:
        """
        Run `command` inside the sandbox and return buffered stdout, stderr,
        and exit_code once the process finishes.

        `command` may be a string (passed to a shell via ``sh -c``) or a list of
        strings (executed directly as argv, each element a literal argument with
        no shell, word-splitting, or expansion).

        `env` is merged on top of the sandbox config's environment, overriding
        entries with the same name. When omitted, the sandbox config
        environment is used as-is.
        """
        body: dict[str, object] = {"command": command}
        if cwd is not None:
            body["cwd"] = cwd
        if env is not None:
            body["env"] = env
        res = await self._client.post(
            f"{self.api_server_url}/v1/{self.key}/exec",
            json=body,
        )
        if not res.is_success:
            raise _to_error(res, "exec")
        return res.json()

    async def exec_stream(
        self,
        command: Union[str, list[str]] = "",
        cwd: Optional[str] = None,
        tty: bool = False,
        env: Optional[dict[str, str]] = None,
    ) -> ExecProcess:
        """
        Run `command` inside the sandbox and return an ExecProcess handle for
        interactive use: stream output via ``exec.pipes``, send input via
        ``exec.write_stdin()``, and await the result via ``exec.exit_code``.

        `command` may be a string (passed to a shell via ``sh -c``) or a list of
        strings (executed directly as argv, each element a literal argument with
        no shell, word-splitting, or expansion).

        Pass an empty `command` to attach to the sandbox entrypoint's terminal
        instead of running a new command — this requires the sandbox to have
        been created with ``tty=True``. The stream stays open until the
        entrypoint exits or you disconnect.

        `env` is merged on top of the sandbox config's environment, overriding
        entries with the same name. When omitted, the sandbox config
        environment is used as-is.

        Resolves once the process is ready, so ``write_stdin`` is safe to call.
        """
        exec_id = str(uuid.uuid4())
        stream_url = f"{self.api_server_url}/v1/{self.key}/exec-stream/{exec_id}"
        stdin_url = f"{self.api_server_url}/v1/{self.key}/exec-stream/{exec_id}/stdin"

        body: dict[str, object] = {}
        if command:
            body["command"] = command
        if cwd is not None:
            body["cwd"] = cwd
        if env is not None:
            body["env"] = env
        if tty:
            body["tty"] = tty

        queue: asyncio.Queue[Optional[dict[str, str]]] = asyncio.Queue()
        exit_future: asyncio.Future[int] = asyncio.get_running_loop().create_future()
        ready = asyncio.Event()
        start_error: list[Exception] = []

        client = self._client

        async def _reader() -> None:
            sse_timeout = httpx.Timeout(None, connect=_FETCH_TIMEOUT.connect)
            try:
                async with client.stream(
                    "POST",
                    stream_url,
                    json=body,
                    headers={"accept": "text/event-stream"},
                    timeout=sse_timeout,
                ) as res:
                    if not res.is_success:
                        await res.aread()
                        start_error.append(_to_error(res, "exec_stream"))
                        ready.set()
                        return
                    ready.set()
                    async for frame in parse_sse(res, None):
                        event: dict[str, object] = json.loads(frame.data)
                        etype = event.get("type")
                        if etype == "stdout":
                            await queue.put({"stdout": str(event["text"])})
                        elif etype == "stderr":
                            await queue.put({"stderr": str(event["text"])})
                        elif etype == "exit":
                            if not exit_future.done():
                                exit_future.set_result(int(event["code"]))  # type: ignore[arg-type]
                            await queue.put(None)
            except Exception as exc:
                if not ready.is_set():
                    start_error.append(exc)
                    ready.set()
                if not exit_future.done():
                    exit_future.set_exception(exc)
                await queue.put(None)

        asyncio.create_task(_reader())
        await ready.wait()
        if start_error:
            raise start_error[0]

        async def _pipes() -> AsyncGenerator[dict[str, str], None]:
            while True:
                item = await queue.get()
                if item is None:
                    return
                yield item

        async def _write_stdin(data: str) -> None:
            res = await client.post(stdin_url, json={"data": data})
            if not res.is_success:
                raise _to_error(res, "exec_stream_stdin")

        return ExecProcess(
            id=exec_id,
            pipes=_pipes(),
            exit_code=exit_future,
            write_stdin_fn=_write_stdin,
        )

    async def list_directory(self, path: str) -> list[dict[str, object]]:
        """
        List the immediate children of a directory under a sandbox mount.
        `path` is the agent-visible absolute path (e.g. `/workspace`).
        Returns a list of entries with `name`, `path`, `is_dir`, and `size`.
        """
        res = await self._client.get(
            f"{self.api_server_url}/v1/{self.key}/directories",
            params={"path": path},
        )
        if not res.is_success:
            raise _to_error(res, "list_directory")
        return res.json()["entries"]

    def _file_url(self, path: str) -> str:
        """
        Build the `/file` URL for an agent-visible absolute path, carrying it as
        trailing URL segments (e.g. `/file/workspace/data.csv`). Each segment is
        escaped while the `/` separators are preserved, so a nested path with
        arbitrarily many segments round-trips intact.
        """
        segments = "/".join(quote(seg, safe="") for seg in path.split("/") if seg)
        return f"{self.api_server_url}/v1/{self.key}/file/{segments}"

    async def read_file(self, path: str) -> bytes:
        """
        Download a file from a sandbox mount. `path` is the agent-visible
        absolute path (e.g. `/workspace/data.csv`). Returns the raw bytes.
        """
        res = await self._client.get(self._file_url(path))
        if not res.is_success:
            raise _to_error(res, "read_file")
        return res.content

    async def write_file(
        self,
        path: str,
        content: Union[bytes, str],
    ) -> dict[str, object]:
        """
        Upload `content` as a file to `path`, the agent-visible absolute path
        (e.g. `/workspace/data.csv`), which must resolve beneath one of the
        configured `fs[].mount` paths. Returns the agent-visible path and byte
        count the server reports.
        """
        if isinstance(content, str):
            content = content.encode("utf-8")
        res = await self._client.post(
            self._file_url(path),
            content=content,
            headers={"Content-Type": "application/octet-stream"},
        )
        if not res.is_success:
            raise _to_error(res, "write_file")
        return res.json()

    async def delete_file(self, path: str) -> None:
        """
        Delete a file or empty directory at `path` inside a sandbox mount.
        `path` is the agent-visible absolute path (e.g. `/workspace/data.csv`).
        """
        res = await self._client.delete(self._file_url(path))
        if not res.is_success:
            raise _to_error(res, "delete_file")

    async def get_events_stream(
        self,
        last_event_id: Optional[int] = None,
        abort: Optional[asyncio.Event] = None,
        max_retries: int = 3,
    ) -> AsyncGenerator[SandboxEvent, None]:
        """
        Stream the sandbox's activity events (egress, filesystem, exec, stdio,
        resource usage) as they happen.

        Auto-resumes across transient disconnects without dropping events,
        reconnecting up to ``max_retries`` times. Resume from a known position
        with ``last_event_id``. Stop the stream by setting ``abort`` or
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
            f"{self.api_server_url}/v1/{self.key}/events",
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
