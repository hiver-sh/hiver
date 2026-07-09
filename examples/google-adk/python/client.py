import asyncio
import json
import httpx
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    # The sandbox launch config: the image built by `hiver bundle ./agent` plus
    # the egress policy that injects the API key at the proxy. Read from the same
    # agent/.hiver.json the image was bundled from, so the key never lives in the
    # sandbox.
    with open("agent/.hiver.json") as f:
        config = SandboxConfig.model_validate(json.load(f))

    # Provision (or reuse) the sandbox from the built image, then drive its
    # in-sandbox server over the Hiver client.
    sandbox = await get_or_create_sandbox("google-adk-py", config)

    async with httpx.AsyncClient() as http:
        res = await http.post(
            f"{sandbox.proxy_url(3000)}chat",
            json={"prompt": "Create /workspace/fib.py and run it."},
        )
        print(res.json()["reply"])


asyncio.run(main())
