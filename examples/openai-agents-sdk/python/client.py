import asyncio
import json
import os
import httpx
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    # The agent authenticates with your OpenAI API key, read from the
    # environment. Running without it exits with an error.
    api_key = os.environ.get("OPENAI_API_KEY")
    if not api_key:
        raise SystemExit("Set OPENAI_API_KEY to run this example.")

    # Launch config from agent/.hiver.json (image + egress policy). Inject the
    # key into the egress override so it's applied at the proxy on the way to
    # api.openai.com — never written into the sandbox itself. The .hiver.json
    # keeps only a placeholder.
    with open("agent/.hiver.json") as f:
        config = SandboxConfig.model_validate(json.load(f))
    for rule in config.egress or []:
        if rule.host == "api.openai.com" and rule.override and rule.override.headers:
            rule.override.headers["Authorization"] = f"Bearer {api_key}"

    sandbox = await get_or_create_sandbox("openai-agents-sdk-py", config)

    # No timeout: a fresh sandbox boots and sandboxd holds the request until the
    # server is ready, then the agent works — well past httpx's short default.
    async with httpx.AsyncClient(timeout=None) as http:
        res = await http.post(
            f"{sandbox.proxy_url(3000)}chat",
            json={"prompt": "Create /workspace/fib.py and run it."},
        )
        res.raise_for_status()
        print(res.json()["reply"])


asyncio.run(main())
