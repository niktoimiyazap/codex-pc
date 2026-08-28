from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import time
from pathlib import Path
from typing import Any

import mcp.types as types

from .app_server import CodexAppServerClient
from .computer import perform as perform_computer_action
from .config import Settings
from .file_editing import FileEditError, apply_exact_edits, atomic_write, read_snapshot, unified_diff
from .mcp_inventory import McpInventoryService
from .security import PathPolicy


def _annotations(*, read_only: bool, destructive: bool = False, open_world: bool = False) -> types.ToolAnnotations:
    return types.ToolAnnotations(
        readOnlyHint=read_only,
        destructiveHint=destructive,
        idempotentHint=read_only,
        openWorldHint=open_world,
    )


def _absolute_path_schema(description: str) -> dict[str, Any]:
    return {"type": "string", "description": description}


def connector_tools(settings: Settings) -> list[types.Tool]:
    tools = [
        types.Tool(
            name="connector_status",
            description="Returns connector and original Codex app-server health metadata.",
            inputSchema={"type": "object", "properties": {}, "additionalProperties": False},
            annotations=_annotations(read_only=True),
        ),
        types.Tool(
            name="fs_read_file",
            description="Reads a file through the original Codex app-server fs/readFile method.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": _absolute_path_schema("Absolute file path."),
                    "encoding": {
                        "type": "string",
                        "enum": ["utf-8", "utf-8-sig", "utf-16-le", "cp1251", "cp866", "base64"],
                        "default": "utf-8",
                    },
                },
                "required": ["path"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=True),
        ),
        types.Tool(
            name="fs_edit_file",
            description="Edits exact text fragments in an existing text file. Read first and pass sha256.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": _absolute_path_schema("Absolute existing text file path."),
                    "expected_sha256": {"type": "string", "minLength": 64, "maxLength": 64},
                    "encoding": {"type": "string", "enum": ["utf-8", "utf-8-sig", "utf-16-le"], "default": "utf-8"},
                    "edits": {
                        "type": "array",
                        "minItems": 1,
                        "items": {
                            "type": "object",
                            "properties": {
                                "old_text": {"type": "string", "minLength": 1},
                                "new_text": {"type": "string"},
                                "expected_count": {"type": "integer", "minimum": 1, "default": 1},
                                "replace_all": {"type": "boolean", "default": False},
                            },
                            "required": ["old_text", "new_text"],
                            "additionalProperties": False,
                        },
                    },
                    "dry_run": {"type": "boolean", "default": False},
                },
                "required": ["path", "expected_sha256", "edits"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True),
        ),
        types.Tool(
            name="fs_write_file",
            description="Creates or intentionally replaces a complete file. Use fs_edit_file for partial edits.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": _absolute_path_schema("Absolute file path."),
                    "content": {"type": "string"},
                    "encoding": {
                        "type": "string",
                        "enum": ["utf-8", "utf-8-sig", "utf-16-le", "cp1251", "cp866", "base64"],
                        "default": "utf-8",
                    },
                },
                "required": ["path", "content"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True),
        ),
        types.Tool(
            name="fs_read_directory",
            description="Lists direct children through the original Codex app-server fs/readDirectory method.",
            inputSchema={
                "type": "object",
                "properties": {"path": _absolute_path_schema("Absolute directory path.")},
                "required": ["path"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=True),
        ),
        types.Tool(
            name="fs_create_directory",
            description="Creates a directory through the original Codex app-server fs/createDirectory method.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": _absolute_path_schema("Absolute directory path."),
                    "recursive": {"type": "boolean", "default": True},
                },
                "required": ["path"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False),
        ),
        types.Tool(
            name="fs_copy",
            description="Copies a file or directory through the original Codex app-server fs/copy method.",
            inputSchema={
                "type": "object",
                "properties": {
                    "source_path": _absolute_path_schema("Absolute source path."),
                    "destination_path": _absolute_path_schema("Absolute destination path."),
                    "recursive": {"type": "boolean", "default": False},
                },
                "required": ["source_path", "destination_path"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False),
        ),
        types.Tool(
            name="fs_remove",
            description="Removes a file or directory through the original Codex app-server fs/remove method.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": _absolute_path_schema("Absolute path to remove."),
                    "recursive": {"type": "boolean", "default": True},
                    "force": {"type": "boolean", "default": True},
                },
                "required": ["path"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True),
        ),
        types.Tool(
            name="command_exec",
            description=(
                "Runs an argv command through the original Codex app-server command/exec method. "
                "The connector does not spawn or manage the process itself."
            ),
            inputSchema={
                "type": "object",
                "properties": {
                    "command": {
                        "type": "array",
                        "items": {"type": "string"},
                        "minItems": 1,
                    },
                    "cwd": {"type": "string"},
                    "timeout_ms": {"type": "integer", "minimum": 1},
                    "output_bytes_cap": {"type": "integer", "minimum": 0},
                    "env": {
                        "type": "object",
                        "additionalProperties": {"type": ["string", "null"]},
                    },
                },
                "required": ["command"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True, open_world=True),
        ),
        types.Tool(
            name="computer",
            description=(
                "Controls the Windows desktop. Actions: screenshot, screen_info, move, click, scroll, type, keypress."
            ),
            inputSchema={
                "type": "object",
                "properties": {
                    "action": {
                        "type": "string",
                        "enum": ["screenshot", "screen_info", "move", "click", "scroll", "type", "keypress"],
                    },
                    "x": {"type": "integer"},
                    "y": {"type": "integer"},
                    "duration_ms": {"type": "integer", "minimum": 0, "maximum": 10000, "default": 0},
                    "button": {"type": "string", "enum": ["left", "right", "middle"], "default": "left"},
                    "clicks": {"type": "integer", "minimum": 1, "maximum": 3, "default": 1},
                    "delta_x": {"type": "integer", "default": 0},
                    "delta_y": {"type": "integer", "default": 0},
                    "text": {"type": "string"},
                    "interval_ms": {"type": "integer", "minimum": 0, "maximum": 1000, "default": 0},
                    "keys": {
                        "oneOf": [
                            {"type": "string"},
                            {"type": "array", "items": {"type": "string"}, "minItems": 1},
                        ]
                    },
                },
                "required": ["action"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True),
        ),
        types.Tool(
            name="mcp_discover",
            description="Discovers MCP servers and tools configured in Codex.",
            inputSchema={
                "type": "object",
                "properties": {
                    "query": {"type": "string", "default": ""},
                    "server_name": {"type": "string"},
                    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50},
                    "refresh": {"type": "boolean", "default": False},
                },
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=True, open_world=True),
        ),
        types.Tool(
            name="mcp_call",
            description="Calls a configured MCP tool through the original Codex app-server.",
            inputSchema={
                "type": "object",
                "properties": {
                    "server_name": {"type": "string"},
                    "tool_name": {"type": "string"},
                    "arguments": {"type": "object", "additionalProperties": True},
                },
                "required": ["server_name", "tool_name"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=False, destructive=True, open_world=True),
        ),
    ]
    if settings.tool_profile == "core":
        return tools
    return tools + gateway_tools()


def gateway_tools() -> list[types.Tool]:
    return [
        types.Tool(
            name="mcp_list_servers",
            description="Compatibility alias for listing MCP servers configured in Codex.",
            inputSchema={
                "type": "object",
                "properties": {"refresh": {"type": "boolean", "default": False}},
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=True, open_world=True),
        ),
        types.Tool(
            name="mcp_list_tools",
            description="Compatibility alias for listing tools from one Codex MCP server.",
            inputSchema={
                "type": "object",
                "properties": {
                    "server_name": {"type": "string"},
                    "refresh": {"type": "boolean", "default": False},
                },
                "required": ["server_name"],
                "additionalProperties": False,
            },
            annotations=_annotations(read_only=True, open_world=True),
        ),
    ]


class CodexTools:
    def __init__(self, settings: Settings, client: CodexAppServerClient):
        self.settings = settings
        self.client = client
        self.policy = PathPolicy(settings)
        self.started_at = time.monotonic()
        self._tools = connector_tools(settings)
        self.names = {tool.name for tool in self._tools}
        self.inventory = McpInventoryService(
            client,
            settings.state_dir / "mcp_inventory.json",
            ttl_sec=settings.mcp_inventory_ttl_sec,
        )

    def list_tools(self) -> list[types.Tool]:
        return list(self._tools)

    def _path(self, value: str) -> str:
        return str(self.policy.resolve(value, base=self.settings.workspace))

    async def shutdown(self) -> None:
        await self.inventory.shutdown()

    async def call(
        self,
        name: str,
        args: dict[str, Any],
        *,
        session_key: str = "default",
        call_id: str | None = None,
    ) -> Any:
        if name == "connector_status":
            return {
                "connector": "ok",
                "app_server_running": self.client.running,
                "uptime_sec": round(time.monotonic() - self.started_at, 3),
                "execution_backend": "codex app-server",
                "local_desktop_control": True,
            }
        if name == "fs_read_file":
            path = self._path(str(args["path"]))
            result = await self.client.request("fs/readFile", {"path": path})
            data = base64.b64decode(str(result["dataBase64"]), validate=True)
            encoding = str(args.get("encoding", "utf-8"))
            metadata = {"path": path, "sha256": hashlib.sha256(data).hexdigest(), "size_bytes": len(data)}
            if encoding == "base64":
                return {**metadata, "data_base64": result["dataBase64"]}
            text = data.decode(encoding)
            crlf = text.count("\r\n")
            lf = text.count("\n") - crlf
            cr = text.count("\r") - crlf
            newline = (
                "crlf" if crlf >= lf and crlf >= cr and crlf else "lf" if lf >= cr and lf else "cr" if cr else "none"
            )
            return {
                **metadata,
                "content": text,
                "encoding": encoding,
                "newline": newline,
                "final_newline": text.endswith(("\n", "\r")),
            }
        if name == "fs_edit_file":
            path = Path(self._path(str(args["path"])))
            self.policy.ensure_writable(path)
            encoding = str(args.get("encoding", "utf-8"))
            snapshot = read_snapshot(path, encoding=encoding)
            expected_sha256 = str(args["expected_sha256"]).lower()
            if snapshot.sha256 != expected_sha256:
                raise FileEditError("STALE_FILE", "File hash does not match expected_sha256")
            updated, replacements = apply_exact_edits(snapshot.text, list(args["edits"]))
            diff = unified_diff(path, snapshot.text, updated)
            new_data = updated.encode(encoding)
            new_sha256 = hashlib.sha256(new_data).hexdigest()
            dry_run = bool(args.get("dry_run", False))
            if not dry_run and new_data != snapshot.data:
                atomic_write(path, new_data, expected_sha256=snapshot.sha256)
            return {
                "path": str(path),
                "changed": new_data != snapshot.data,
                "dry_run": dry_run,
                "replacements": replacements,
                "old_sha256": snapshot.sha256,
                "new_sha256": new_sha256,
                "encoding": snapshot.encoding,
                "newline": snapshot.newline,
                "final_newline": snapshot.final_newline,
                "diff": diff,
            }
        if name == "fs_write_file":
            path = self._path(str(args["path"]))
            encoding = str(args.get("encoding", "utf-8"))
            content = str(args["content"])
            data = base64.b64decode(content, validate=True) if encoding == "base64" else content.encode(encoding)
            result = await self.client.request(
                "fs/writeFile",
                {"path": path, "dataBase64": base64.b64encode(data).decode("ascii")},
            )
            return {"path": path, "written": True, "size_bytes": len(data), "result": result}
        if name == "fs_read_directory":
            path = self._path(str(args["path"]))
            return await self.client.request("fs/readDirectory", {"path": path})
        if name == "fs_create_directory":
            path = self._path(str(args["path"]))
            return await self.client.request(
                "fs/createDirectory",
                {"path": path, "recursive": bool(args.get("recursive", True))},
            )
        if name == "fs_copy":
            source = self._path(str(args["source_path"]))
            destination = self._path(str(args["destination_path"]))
            return await self.client.request(
                "fs/copy",
                {
                    "sourcePath": source,
                    "destinationPath": destination,
                    "recursive": bool(args.get("recursive", False)),
                },
            )
        if name == "fs_remove":
            path = self._path(str(args["path"]))
            self.policy.ensure_writable(Path(path))
            return await self.client.request(
                "fs/remove",
                {
                    "path": path,
                    "recursive": bool(args.get("recursive", True)),
                    "force": bool(args.get("force", True)),
                },
            )
        if name == "command_exec":
            command = [str(value) for value in args["command"]]
            stream_supported = hasattr(self.client, "begin_command_stream") and hasattr(self.client, "end_command_stream")
            process_id = f"{call_id or 'command'}-{time.monotonic_ns()}"
            payload: dict[str, Any] = {"command": command}
            if stream_supported:
                payload["processId"] = process_id
                payload["streamStdoutStderr"] = True
            if args.get("cwd") is not None:
                payload["cwd"] = self._path(str(args["cwd"]))
            if args.get("timeout_ms") is not None:
                payload["timeoutMs"] = int(args["timeout_ms"])
            if args.get("output_bytes_cap") is not None:
                payload["outputBytesCap"] = int(args["output_bytes_cap"])
            if args.get("env") is not None:
                payload["env"] = dict(args["env"])
            if not stream_supported:
                return await self.client.request("command/exec", payload)
            self.client.begin_command_stream(process_id, call_id or process_id)
            try:
                result = await self.client.request("command/exec", payload)
            finally:
                streamed = self.client.end_command_stream(process_id)
            if isinstance(result, dict):
                merged = dict(result)
                for stream in ("stdout", "stderr"):
                    merged[stream] = streamed[stream] + str(result.get(stream) or "")
                return merged
            return result
        if name == "computer":
            action = str(args.get("action") or "")
            payload = {key: value for key, value in args.items() if key != "action"}
            return await asyncio.to_thread(perform_computer_action, action, payload)
        if name == "mcp_discover":
            return await self.inventory.discover(
                query=str(args.get("query", "")),
                server_name=str(args["server_name"]) if args.get("server_name") else None,
                limit=int(args.get("limit", 50)),
                refresh=bool(args.get("refresh", False)),
            )
        if name == "mcp_list_servers":
            return await self.inventory.list_servers(refresh=bool(args.get("refresh", False)))
        if name == "mcp_list_tools":
            return await self.inventory.list_tools(
                str(args["server_name"]),
                refresh=bool(args.get("refresh", False)),
            )
        if name == "mcp_call":
            thread_id = await self.client.ensure_thread(session_key)
            return await self.client.request(
                "mcpServer/tool/call",
                {
                    "threadId": thread_id,
                    "server": str(args["server_name"]),
                    "tool": str(args["tool_name"]),
                    "arguments": dict(args.get("arguments") or {}),
                },
            )
        raise KeyError(f"Unknown tool: {name}")


def json_text(value: Any, max_chars: int) -> str:
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, indent=2)
    if len(text) > max_chars:
        return text[:max_chars] + "\n... output truncated ..."
    return text
