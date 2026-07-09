from flask import Flask, request, jsonify
from claude_agent_sdk import query, ClaudeAgentOptions, ResultMessage

app = Flask(__name__)


@app.post("/chat")
async def chat():
    reply = ""
    async for message in query(
        prompt=request.json["prompt"],
        options=ClaudeAgentOptions(
            model="claude-opus-4-8",
            cwd="/workspace",
            allowed_tools=[
                "Bash",
                "Read",
                "Write",
                "Edit",
                "Glob",
                "Grep",
                "WebSearch",
            ],
            permission_mode="bypassPermissions",
        ),
    ):
        if (
            isinstance(message, ResultMessage)
            and message.subtype == "success"
        ):
            reply = message.result
    return jsonify(reply=reply)


app.run(host="0.0.0.0", port=3000)
