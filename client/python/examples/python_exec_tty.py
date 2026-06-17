# Run an interactive Python REPL inside the sandbox with a TTY, writing to
# stdin to drive it programmatically.
#
# Run with: python examples/python_exec_tty.py
import asyncio
import sys

import hiver

LINES = [
    "x = 6 * 7",
    "print('the answer is', x)",
    "exit()",
]


async def main() -> None:
    sandbox = await hiver.get_or_create_sandbox(
        "hive-python-exec-tty",
        hiver.SandboxConfig(
            image="hiversh/python:3.13-alpine",
            entrypoint=["tail", "-f", "/dev/null"],
            fs=[
                hiver.LocalFileSystem(
                    backend="local",
                    mount="/workspace",
                    acls=[hiver.ACLRule(path="/workspace/**", access="rw")],
                )
            ],
            ttl=0,
        ),
    )

    exec = await sandbox.exec_stream("python3", cwd="/workspace", tty=True)

    async def feed_stdin() -> None:
        for line in LINES:
            await exec.write_stdin(line + "\r")

    asyncio.create_task(feed_stdin())

    async for pipe in exec.pipes:
        if "stdout" in pipe:
            sys.stdout.write(pipe["stdout"])
        if "stderr" in pipe:
            sys.stderr.write(pipe["stderr"])

    print("\nexit code:", await exec.exit_code)


asyncio.run(main())
