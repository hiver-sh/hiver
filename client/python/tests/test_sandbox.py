import asyncio
import json
from typing import Union

import httpx
import pytest
import respx

from hiver.sandbox import Sandbox, SandboxError
from hiver.schemas import SandboxRef

GATEWAY = "http://gateway:10000"
REF = SandboxRef(id="11111111-1111-1111-1111-111111111111", key="sb-1")
SANDBOX_BASE = f"{GATEWAY}/sandbox/{REF.id}"
SANDBOX_V1 = f"{SANDBOX_BASE}/v1/{REF.key}"

MIN_CONFIG = {"fs": [{"backend": "local", "mount": "/workspace"}]}
MIN_APPLY_RESULT = {
    "applied": True,
    "config": {"fs": [{"backend": "local", "mount": "/workspace"}]},
    "changes": {},
}
STDIO_EVENT = {
    "id": 1,
    "timestamp": "2024-01-01T00:00:00Z",
    "type": "stdio",
    "stdout": "hello",
}


def make_sandbox(client: httpx.AsyncClient) -> Sandbox:
    return Sandbox(REF, GATEWAY, client=client)


def sse_body(*events: object) -> bytes:
    return b"".join(f"data: {json.dumps(e)}\n\n".encode() for e in events)


class _MockTransport(httpx.AsyncBaseTransport):
    """Direct transport mock for SSE tests — bypasses the contextvar limitation
    that prevents respx from being visible inside async generators."""

    def __init__(self, *responses: Union[httpx.Response, Exception]) -> None:
        self._queue: list[Union[httpx.Response, Exception]] = list(responses)
        self.requests: list[httpx.Request] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if not self._queue:
            return httpx.Response(500, json={"error": "no more mock responses"})
        result = self._queue.pop(0)
        if isinstance(result, Exception):
            raise result
        return result


def sse_response(*events: object, status: int = 200) -> httpx.Response:
    return httpx.Response(status, content=sse_body(*events))


# ping


@pytest.mark.asyncio
@respx.mock
async def test_ping_sends_get_v1_ping() -> None:
    route = respx.get(f"{SANDBOX_V1}/ping").mock(return_value=httpx.Response(200))
    async with httpx.AsyncClient() as client:
        await make_sandbox(client).ping()
    assert route.called
    assert route.calls[0].request.url == f"{SANDBOX_V1}/ping"


@pytest.mark.asyncio
@respx.mock
async def test_ping_raises_sandbox_error_on_non_200() -> None:
    respx.get(f"{SANDBOX_V1}/ping").mock(
        return_value=httpx.Response(503, json={"error": "service unavailable"})
    )
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).ping()
    assert exc.value.status == 503
    assert exc.value.operation == "ping"


# get_ports


@pytest.mark.asyncio
@respx.mock
async def test_get_ports_sends_get_v1_ports_and_returns_list() -> None:
    route = respx.get(f"{SANDBOX_V1}/ports").mock(
        return_value=httpx.Response(200, json=[8080, 9000])
    )
    async with httpx.AsyncClient() as client:
        ports = await make_sandbox(client).get_ports()
    assert route.called
    assert ports == [8080, 9000]


@pytest.mark.asyncio
@respx.mock
async def test_get_ports_raises_sandbox_error_on_non_200() -> None:
    respx.get(f"{SANDBOX_V1}/ports").mock(
        return_value=httpx.Response(500, json={"error": "internal"})
    )
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).get_ports()
    assert exc.value.status == 500
    assert exc.value.operation == "get_ports"


# get_config


@pytest.mark.asyncio
@respx.mock
async def test_get_config_sends_get_v1_config_and_returns_config() -> None:
    route = respx.get(f"{SANDBOX_V1}/config").mock(
        return_value=httpx.Response(200, json=MIN_CONFIG)
    )
    async with httpx.AsyncClient() as client:
        config = await make_sandbox(client).get_config()
    assert route.called
    assert config.fs[0].mount == "/workspace"  # type: ignore[union-attr]


@pytest.mark.asyncio
@respx.mock
async def test_get_config_raises_sandbox_error_on_non_200() -> None:
    respx.get(f"{SANDBOX_V1}/config").mock(
        return_value=httpx.Response(404, json={"error": "not found"})
    )
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).get_config()
    assert exc.value.status == 404
    assert exc.value.operation == "get_config"


# apply_config


@pytest.mark.asyncio
@respx.mock
async def test_apply_config_sends_put_v1_config_with_json_body() -> None:
    route = respx.put(f"{SANDBOX_V1}/config").mock(
        return_value=httpx.Response(200, json=MIN_APPLY_RESULT)
    )
    from hiver.schemas import SandboxConfig

    config = SandboxConfig.model_validate(MIN_CONFIG)
    async with httpx.AsyncClient() as client:
        await make_sandbox(client).apply_config(config)

    req = route.calls[0].request
    assert req.method == "PUT"
    assert req.headers["content-type"] == "application/json"
    body = json.loads(req.content)
    assert body["fs"][0]["mount"] == "/workspace"


@pytest.mark.asyncio
@respx.mock
async def test_apply_config_returns_apply_result() -> None:
    respx.put(f"{SANDBOX_V1}/config").mock(
        return_value=httpx.Response(200, json=MIN_APPLY_RESULT)
    )
    from hiver.schemas import SandboxConfig

    config = SandboxConfig.model_validate(MIN_CONFIG)
    async with httpx.AsyncClient() as client:
        result = await make_sandbox(client).apply_config(config)
    assert result.applied is True
    assert result.config.fs[0].mount == "/workspace"  # type: ignore[union-attr]


@pytest.mark.asyncio
@respx.mock
async def test_apply_config_raises_sandbox_error_on_non_200() -> None:
    respx.put(f"{SANDBOX_V1}/config").mock(
        return_value=httpx.Response(400, json={"error": "bad config"})
    )
    from hiver.schemas import SandboxConfig

    config = SandboxConfig.model_validate(MIN_CONFIG)
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).apply_config(config)
    assert exc.value.status == 400
    assert exc.value.operation == "apply_config"


# read_file


@pytest.mark.asyncio
@respx.mock
async def test_read_file_sends_get_with_path_in_url_and_returns_bytes() -> None:
    content = b"hello"
    route = respx.get(f"{SANDBOX_V1}/file/workspace/hello.txt").mock(
        return_value=httpx.Response(200, content=content)
    )
    async with httpx.AsyncClient() as client:
        result = await make_sandbox(client).read_file("/workspace/hello.txt")

    assert result == content
    assert isinstance(result, bytes)
    assert route.calls[0].request.url.path.endswith("/file/workspace/hello.txt")


@pytest.mark.asyncio
@respx.mock
async def test_read_file_raises_sandbox_error_on_non_200() -> None:
    respx.get(f"{SANDBOX_V1}/file/workspace/missing.txt").mock(
        return_value=httpx.Response(404, json={"error": "not found"})
    )
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).read_file("/workspace/missing.txt")
    assert exc.value.status == 404
    assert exc.value.operation == "read_file"


# write_file


@pytest.mark.asyncio
@respx.mock
async def test_write_file_sends_post_with_path_in_url_and_raw_body() -> None:
    route = respx.post(f"{SANDBOX_V1}/file/workspace/hello.txt").mock(
        return_value=httpx.Response(200, json={"path": "/workspace/hello.txt", "bytes": 5})
    )
    async with httpx.AsyncClient() as client:
        result = await make_sandbox(client).write_file("/workspace/hello.txt", b"hello")

    req = route.calls[0].request
    assert req.method == "POST"
    assert req.url.path.endswith("/file/workspace/hello.txt")
    assert req.headers["content-type"] == "application/octet-stream"
    assert req.content == b"hello"
    assert result == {"path": "/workspace/hello.txt", "bytes": 5}


@pytest.mark.asyncio
@respx.mock
async def test_write_file_accepts_str_content() -> None:
    respx.post(f"{SANDBOX_V1}/file/workspace/f.txt").mock(
        return_value=httpx.Response(200, json={"path": "/workspace/f.txt", "bytes": 4})
    )
    async with httpx.AsyncClient() as client:
        result = await make_sandbox(client).write_file("/workspace/f.txt", "data")
    assert result["bytes"] == 4


@pytest.mark.asyncio
@respx.mock
async def test_write_file_raises_sandbox_error_on_non_200() -> None:
    respx.post(f"{SANDBOX_V1}/file/workspace/f.txt").mock(
        return_value=httpx.Response(400, json={"error": "path not mounted"})
    )
    async with httpx.AsyncClient() as client:
        with pytest.raises(SandboxError) as exc:
            await make_sandbox(client).write_file("/workspace/f.txt", b"data")
    assert exc.value.status == 400
    assert exc.value.operation == "write_file"


# get_events_stream
# These tests use _MockTransport directly instead of respx because respx relies on
# context variables that are not inherited by async generators in Python.


@pytest.mark.asyncio
async def test_events_stream_sends_get_with_accept_header() -> None:
    transport = _MockTransport(sse_response(STDIO_EVENT))
    abort = asyncio.Event()
    async with httpx.AsyncClient(transport=transport) as client:
        async for _ in make_sandbox(client).get_events_stream(abort=abort, max_retries=0):
            abort.set()

    req = transport.requests[0]
    assert str(req.url).startswith(f"{SANDBOX_V1}/events")
    assert req.headers["accept"] == "text/event-stream"


@pytest.mark.asyncio
async def test_events_stream_yields_parsed_events() -> None:
    transport = _MockTransport(sse_response(STDIO_EVENT))
    events = []
    abort = asyncio.Event()
    async with httpx.AsyncClient(transport=transport) as client:
        async for evt in make_sandbox(client).get_events_stream(abort=abort):
            events.append(evt)
            abort.set()

    assert len(events) == 1
    assert events[0].id == 1  # type: ignore[union-attr]
    assert events[0].type == "stdio"  # type: ignore[union-attr]
    assert events[0].stdout == "hello"  # type: ignore[union-attr]


@pytest.mark.asyncio
async def test_events_stream_stops_when_abort_is_set() -> None:
    transport = _MockTransport(sse_response(STDIO_EVENT, {**STDIO_EVENT, "id": 2}))
    abort = asyncio.Event()
    events = []
    async with httpx.AsyncClient(transport=transport) as client:
        async for evt in make_sandbox(client).get_events_stream(abort=abort):
            events.append(evt)
            abort.set()

    assert len(events) == 1


@pytest.mark.asyncio
async def test_events_stream_reconnects_and_passes_last_event_id() -> None:
    event1 = {**STDIO_EVENT, "id": 5}
    event2 = {**STDIO_EVENT, "id": 6}
    transport = _MockTransport(sse_response(event1), sse_response(event2))
    events = []
    async with httpx.AsyncClient(transport=transport) as client:
        async for evt in make_sandbox(client).get_events_stream(max_retries=1):
            events.append(evt)

    assert len(events) == 2
    assert len(transport.requests) == 2
    assert transport.requests[1].url.params["lastEventId"] == "5"


@pytest.mark.asyncio
async def test_events_stream_stops_after_max_retries_on_error() -> None:
    # max_retries=1 → 2 total attempts (retry=0 and retry=1), then retry=2 > 1 exits
    transport = _MockTransport(
        httpx.Response(500, json={"error": "internal server error"}),
        httpx.Response(500, json={"error": "internal server error"}),
    )
    events = []
    async with httpx.AsyncClient(transport=transport) as client:
        async for evt in make_sandbox(client).get_events_stream(max_retries=1):
            events.append(evt)

    assert len(events) == 0
    assert len(transport.requests) == 2
