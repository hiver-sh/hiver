import asyncio
import os
from hiver import get_or_create_sandbox, SandboxConfig


async def main():
    api_key = os.environ.get("OPENAI_API_KEY")
    if not api_key:
        raise SystemExit("Set OPENAI_API_KEY to run this example.")

    sandbox = await get_or_create_sandbox("codex", SandboxConfig(
        image="codex",
        env={"OPENAI_API_KEY": api_key},
    ))

    # `codex exec` runs a single prompt non-interactively and prints the result.
    result = await sandbox.exec(["codex", "exec", "Add tests for src/parser.ts"], cwd="/workspace")
    print(result["stdout"])


asyncio.run(main())
