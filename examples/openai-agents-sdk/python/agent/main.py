import subprocess
from pathlib import Path
from flask import Flask, request, jsonify
from agents import Agent, Runner, function_tool


# The SDK ships no local file tools, so define them as function tools. The
# agent runs inside the sandbox, so they resolve against its filesystem.
@function_tool
def bash(command: str) -> str:
    """Run a shell command in /workspace and return its output."""
    result = subprocess.run(
        ["bash", "-lc", command],
        cwd="/workspace",
        capture_output=True,
        text=True,
    )
    return result.stdout or result.stderr


@function_tool
def read_file(path: str) -> str:
    """Read a file from /workspace."""
    return (Path("/workspace") / path).read_text()


@function_tool
def write_file(path: str, content: str) -> str:
    """Write content to a file in /workspace."""
    (Path("/workspace") / path).write_text(content)
    return f"wrote {path}"


agent = Agent(
    name="coder",
    model="gpt-5",
    instructions="You are a coding agent working in /workspace.",
    tools=[bash, read_file, write_file],
)

app = Flask(__name__)


@app.post("/chat")
async def chat():
    result = await Runner.run(agent, request.json["prompt"])
    return jsonify(reply=result.final_output)


app.run(host="0.0.0.0", port=3000)
