import asyncio
import os
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    sandbox = await get_or_create_sandbox("copilot", SandboxConfig(
        image="copilot",
        env={"GITHUB_TOKEN": os.environ["GITHUB_TOKEN"]},
    ))

    # `copilot -p` runs a single prompt non-interactively and prints the result.
    result = await sandbox.exec(
        ["copilot", "-p", "Explain what src/server.ts does", "--allow-all-tools"],
        cwd="/workspace",
    )
    print(result["stdout"])


asyncio.run(main())
