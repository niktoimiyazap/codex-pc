from __future__ import annotations

import asyncio
import json
import os
import sys
import tempfile
from pathlib import Path

from mcp.client.session import ClientSession
from mcp.client.stdio import StdioServerParameters, stdio_client

ROOT = Path(__file__).resolve().parents[1]


async def run() -> None:
    with tempfile.TemporaryDirectory() as temp:
        env = os.environ.copy()
        env["CODEXPC_STATE_DIR"] = temp
        params = StdioServerParameters(
            command=sys.executable,
            args=["-m", "codexpc_connector"],
            cwd=str(ROOT),
            env=env,
        )
        async with stdio_client(params) as (read_stream, write_stream):  # noqa: SIM117
            async with ClientSession(read_stream, write_stream) as session:
                await session.initialize()
                listed = await session.list_tools()
                names = {tool.name for tool in listed.tools}
                required = {
                    "connector_status",
                    "fs_read_file",
                    "fs_write_file",
                    "fs_read_directory",
                    "fs_create_directory",
                    "fs_copy",
                    "fs_remove",
                    "command_exec",
                    "computer",
                    "mcp_discover",
                    "mcp_call",
                }
                missing = required - names
                if missing:
                    raise RuntimeError(f"Missing tools: {sorted(missing)}")

                status = await session.call_tool("connector_status", {})
                if not status.content or status.content[0].type != "text":
                    raise RuntimeError("connector_status returned no text")
                payload = json.loads(status.content[0].text)
                if payload.get("connector") != "ok":
                    raise RuntimeError(f"Unexpected connector status: {payload}")

                discovery = await session.call_tool("mcp_discover", {"limit": 1})
                if not discovery.content or discovery.content[0].type != "text":
                    raise RuntimeError("mcp_discover returned no text")
                server_payload = json.loads(discovery.content[0].text)
                if not isinstance(server_payload, (dict, list)):
                    raise RuntimeError("mcp_discover returned unexpected JSON")
                serialized = json.dumps(server_payload)
                if "github_pat_" in serialized or 'TELEGRAM_API_HASH":' in serialized:
                    raise RuntimeError("Discovery response appears to contain a secret")
                if payload.get("execution_backend") != "codex app-server":
                    raise RuntimeError(f"Unexpected connector backend: {payload}")
                print(f"stdio smoke passed: {len(names)} explicit tools")


if __name__ == "__main__":
    asyncio.run(run())
