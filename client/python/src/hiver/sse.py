from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import AsyncGenerator, Optional

import httpx


@dataclass
class SSEFrame:
    data: str
    last_event_id: Optional[str] = field(default=None)


async def parse_sse(
    response: httpx.Response,
    abort: Optional[asyncio.Event] = None,
) -> AsyncGenerator[SSEFrame, None]:
    buffer = ""
    last_event_id: Optional[str] = None

    async for chunk in response.aiter_bytes():
        if abort is not None and abort.is_set():
            break

        buffer += chunk.decode("utf-8")
        buffer = buffer.replace("\r\n", "\n")

        while "\n\n" in buffer:
            if abort is not None and abort.is_set():
                return
            sep = buffer.index("\n\n")
            frame_text = buffer[:sep]
            buffer = buffer[sep + 2:]

            data_lines: list[str] = []
            for line in frame_text.split("\n"):
                if not line or line.startswith(":"):
                    continue
                colon = line.find(":")
                if colon == -1:
                    field_name, value = line, ""
                else:
                    field_name = line[:colon]
                    value = line[colon + 1:]
                    if value.startswith(" "):
                        value = value[1:]

                if field_name == "data":
                    data_lines.append(value)
                elif field_name == "id":
                    last_event_id = value

            if data_lines:
                yield SSEFrame(data="\n".join(data_lines), last_event_id=last_event_id)
