import asyncio
import os
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    sandbox = await get_or_create_sandbox("codex", SandboxConfig(
        image="codex",
        env={"OPENAI_API_KEY": os.environ["OPENAI_API_KEY"]},
    ))

    # `codex exec` runs a single prompt non-interactively and prints the result.
    result = await sandbox.exec(["codex", "exec", "Add tests for src/parser.ts"], cwd="/workspace")
    print(result["stdout"])


asyncio.run(main())
