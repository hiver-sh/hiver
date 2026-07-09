import asyncio
import os
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    sandbox = await get_or_create_sandbox("claude-code", SandboxConfig(
        image="claude",
        env={"ANTHROPIC_API_KEY": os.environ["ANTHROPIC_API_KEY"]},
    ))

    # `claude -p` runs a single prompt non-interactively and prints the result.
    result = await sandbox.exec(["claude", "-p", "Fix the bug in src/main.ts"], cwd="/workspace")
    print(result["stdout"])


asyncio.run(main())
