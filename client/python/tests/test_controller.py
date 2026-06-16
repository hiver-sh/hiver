import pytest
import httpx
import respx

from hiver.controller import DEFAULT_GATEWAY_URL, get_or_create_sandbox, shutdown
from hiver.sandbox import Sandbox, SandboxError
from hiver.schemas import SandboxConfig, SandboxRef

SANDBOX_ID = "11111111-1111-1111-1111-111111111111"
SANDBOX_REF = {"id": SANDBOX_ID, "key": "test-sandbox"}
BASE_CONFIG = SandboxConfig.model_validate(
    {"fs": [{"backend": "local", "mount": "/workspace"}]}
)


def make_sandbox() -> Sandbox:
    return Sandbox(SandboxRef(id=SANDBOX_ID, key="test-sandbox"), DEFAULT_GATEWAY_URL)


# get_or_create_sandbox


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_sends_put_with_json_body() -> None:
    route = respx.put(
        f"{DEFAULT_GATEWAY_URL}/controller/v1/sandboxes/test-sandbox"
    ).mock(return_value=httpx.Response(200, json=SANDBOX_REF))

    await get_or_create_sandbox("test-sandbox", BASE_CONFIG, timeout_s=0)

    assert route.called
    req = route.calls[0].request
    assert req.method == "PUT"
    assert req.headers["content-type"] == "application/json"
    import json
    body = json.loads(req.content)
    assert body["fs"][0]["mount"] == "/workspace"


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_returns_sandbox_with_correct_id_and_endpoint_on_200() -> None:
    respx.put(f"{DEFAULT_GATEWAY_URL}/controller/v1/sandboxes/test-sandbox").mock(
        return_value=httpx.Response(200, json=SANDBOX_REF)
    )
    sandbox = await get_or_create_sandbox("test-sandbox", BASE_CONFIG, timeout_s=0)
    assert isinstance(sandbox, Sandbox)
    assert sandbox.id == SANDBOX_ID
    assert sandbox.key == "test-sandbox"
    assert sandbox.api_server_url == f"{DEFAULT_GATEWAY_URL}/sandbox/{SANDBOX_ID}"


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_returns_sandbox_on_201() -> None:
    respx.put(f"{DEFAULT_GATEWAY_URL}/controller/v1/sandboxes/test-sandbox").mock(
        return_value=httpx.Response(201, json=SANDBOX_REF)
    )
    sandbox = await get_or_create_sandbox("test-sandbox", BASE_CONFIG, timeout_s=0)
    assert isinstance(sandbox, Sandbox)


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_uses_custom_gateway_url() -> None:
    route = respx.put("http://custom-gateway:1234/controller/v1/sandboxes/test-sandbox").mock(
        return_value=httpx.Response(200, json=SANDBOX_REF)
    )
    await get_or_create_sandbox(
        "test-sandbox",
        BASE_CONFIG,
        gateway_url="http://custom-gateway:1234",
        timeout_s=0,
    )
    assert route.called


@pytest.mark.asyncio
async def test_get_or_create_sandbox_raises_for_invalid_id() -> None:
    with pytest.raises(ValueError, match="must match"):
        await get_or_create_sandbox("invalid id!", BASE_CONFIG, timeout_s=0)


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_raises_sandbox_error_on_4xx() -> None:
    respx.put(f"{DEFAULT_GATEWAY_URL}/controller/v1/sandboxes/test-sandbox").mock(
        return_value=httpx.Response(409, json={"error": "conflict"})
    )
    with pytest.raises(SandboxError) as exc:
        await get_or_create_sandbox("test-sandbox", BASE_CONFIG, timeout_s=0)
    assert exc.value.status == 409
    assert exc.value.operation == "get_or_create_sandbox"


@pytest.mark.asyncio
@respx.mock
async def test_get_or_create_sandbox_raises_sandbox_error_on_connection_refused() -> None:
    respx.put(f"{DEFAULT_GATEWAY_URL}/controller/v1/sandboxes/test-sandbox").mock(
        side_effect=httpx.ConnectError("Connection refused")
    )
    with pytest.raises(SandboxError) as exc:
        await get_or_create_sandbox("test-sandbox", BASE_CONFIG, timeout_s=0)
    assert exc.value.status == 0
    assert "connection refused" in str(exc.value).lower()


# shutdown


@pytest.mark.asyncio
@respx.mock
async def test_shutdown_sends_post_v1_shutdown() -> None:
    route = respx.post(f"{DEFAULT_GATEWAY_URL}/controller/v1/shutdown/test-sandbox").mock(
        return_value=httpx.Response(204)
    )
    await shutdown(make_sandbox())
    assert route.called
    assert route.calls[0].request.method == "POST"


@pytest.mark.asyncio
@respx.mock
async def test_shutdown_resolves_on_204() -> None:
    respx.post(f"{DEFAULT_GATEWAY_URL}/controller/v1/shutdown/test-sandbox").mock(
        return_value=httpx.Response(204)
    )
    result = await shutdown(make_sandbox())
    assert result is None


@pytest.mark.asyncio
@respx.mock
async def test_shutdown_raises_sandbox_error_on_non_204() -> None:
    respx.post(f"{DEFAULT_GATEWAY_URL}/controller/v1/shutdown/test-sandbox").mock(
        return_value=httpx.Response(404, json={"error": "not found"})
    )
    with pytest.raises(SandboxError) as exc:
        await shutdown(make_sandbox())
    assert exc.value.status == 404
    assert exc.value.operation == "shutdown"


@pytest.mark.asyncio
@respx.mock
async def test_shutdown_raises_sandbox_error_on_connection_refused() -> None:
    respx.post(f"{DEFAULT_GATEWAY_URL}/controller/v1/shutdown/test-sandbox").mock(
        side_effect=httpx.ConnectError("Connection refused")
    )
    with pytest.raises(SandboxError) as exc:
        await shutdown(make_sandbox())
    assert exc.value.status == 0
    assert "connection refused" in str(exc.value).lower()
