import asyncio
import os
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        raise SystemExit("Set ANTHROPIC_API_KEY to run this example.")

    sandbox = await get_or_create_sandbox("claude-code", SandboxConfig(
        image="claude",
        env={"ANTHROPIC_API_KEY": api_key},
    ))

    # `claude -p` runs a single prompt non-interactively and prints the result.
    result = await sandbox.exec(["claude", "-p", "Fix the bug in src/main.ts"], cwd="/workspace")
    print(result["stdout"])


asyncio.run(main())
