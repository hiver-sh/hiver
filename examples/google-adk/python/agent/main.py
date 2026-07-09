import subprocess
from flask import Flask, request, jsonify
from google.adk.agents import Agent
from google.adk.runners import InMemoryRunner
from google.genai import types


def bash(command: str) -> str:
    """Run a shell command in /workspace and return its output."""
    result = subprocess.run(
        ["bash", "-lc", command],
        cwd="/workspace",
        capture_output=True,
        text=True,
    )
    return result.stdout or result.stderr


agent = Agent(
    name="coder",
    model="gemini-2.5-pro",
    instruction="You are a coding agent working in /workspace.",
    tools=[bash],
)

runner = InMemoryRunner(agent=agent, app_name="coder")
app = Flask(__name__)


@app.post("/chat")
async def chat():
    session = await runner.session_service.create_session(
        app_name="coder", user_id="user"
    )
    reply = ""
    async for event in runner.run_async(
        user_id="user",
        session_id=session.id,
        new_message=types.Content(
            role="user", parts=[types.Part(text=request.json["prompt"])]
        ),
    ):
        if event.is_final_response() and event.content:
            reply = event.content.parts[0].text
    return jsonify(reply=reply)


app.run(host="0.0.0.0", port=3000)
