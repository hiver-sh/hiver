# Run a Python function inside the sandbox and stream its output via SSE.
#
# Run with: python examples/python_exec_stream.py
import asyncio
import sys

import hive

SCRIPT = """
import sys, time

def greet(name):
    print(f"Hello, {name}!", flush=True)
    time.sleep(0.5)
    print(f"Hello, {name} again after 500ms!", flush=True)
    time.sleep(0.5)
    print("Bye!", file=sys.stderr, flush=True)

greet("world")
""".strip()


async def main() -> None:
    sandbox = await hive.get_or_create_sandbox(
        "hive-python-exec-stream",
        hive.SandboxConfig(
            image="hiversh/python:3.13-alpine",
            entrypoint="tail -f /dev/null",
            fs=[
                hive.LocalFileSystem(
                    backend="local",
                    mount="/workspace",
                    acls=[hive.ACLRule(path="/workspace/**", access="rw")],
                )
            ],
        ),
    )

    exec = await sandbox.exec_stream(f"python3 -c '{SCRIPT}'", cwd="/workspace")

    async for pipe in exec.pipes:
        if "stdout" in pipe:
            sys.stdout.write("stdout: " + pipe["stdout"])
        if "stderr" in pipe:
            sys.stderr.write("stderr: " + pipe["stderr"])

    print("exit code:", await exec.exit_code)


asyncio.run(main())
