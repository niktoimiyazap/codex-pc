from __future__ import annotations

import asyncio
import base64
import hashlib
import itertools
import logging
import time
from contextlib import suppress
from typing import Any

import mcp.types as types
from mcp.server import Server
from mcp.server.stdio import stdio_server

from .app_server import CodexAppServerClient
from .config import Settings
from .instance_lock import SingleInstanceLock
from .logging_utils import close_logging, configure_logging, log_event
from .security import redact
from .tools import CodexTools, gateway_tools, json_text

_PREVIEW_LIMIT = 240


def _single_line(value: Any, limit: int = _PREVIEW_LIMIT) -> str:
    """Return a compact, redacted preview suitable for structured logs."""
    text = json_text(redact(value), limit * 4) if not isinstance(value, str) else redact(value)
    text = " ".join(str(text).split())
    return text if len(text) <= limit else f"{text[: limit - 1]}…"


def _session_key(server: Server) -> str:
    """Derive a stable, non-sensitive session key from MCP request metadata."""
    try:
        context = server.request_context
        meta = context.meta.model_dump(exclude_none=True) if context.meta else {}
    except (LookupError, AttributeError):
        meta = {}

    candidates: list[tuple[str, Any]] = []

    def collect(value: Any, path: str = "") -> None:
        if isinstance(value, dict):
            for key, item in value.items():
                child = f"{path}.{key}" if path else str(key)
                collect(item, child)
        elif isinstance(value, (str, int)):
            lowered = path.casefold()
            if any(token in lowered for token in ("conversation", "session", "chat", "thread")):
                candidates.append((path, value))

    collect(meta)
    if candidates:
        source = repr(sorted(candidates))
    else:
        # A separate MCP transport session still gets isolation even when the client
        # does not expose a ChatGPT conversation identifier.
        try:
            source = f"transport:{id(server.request_context.session)}"
        except LookupError:
            source = "default"
    return hashlib.sha256(source.encode("utf-8")).hexdigest()[:24]


def _argument_summary(arguments: dict[str, Any]) -> dict[str, Any]:
    """Describe a tool call without dumping potentially sensitive payloads."""
    summary: dict[str, Any] = {
        "argument_keys": sorted(arguments),
        "argument_count": len(arguments),
    }
    if arguments:
        summary["input_preview"] = _single_line(arguments)
    for key in ("filepath", "path", "output", "destination"):
        value = arguments.get(key)
        if isinstance(value, str) and value.strip():
            summary["target_path"] = redact(value)
            break
    for key in ("server_name", "server", "tool_name"):
        value = arguments.get(key)
        if isinstance(value, str) and value.strip():
            summary[key] = redact(value)
    return summary


def _find_media_path(value: Any) -> str | None:
    media_suffixes = {
        ".png",
        ".jpg",
        ".jpeg",
        ".webp",
        ".gif",
        ".bmp",
        ".svg",
        ".mp4",
        ".webm",
        ".mov",
        ".m4v",
        ".avi",
        ".mkv",
        ".mp3",
        ".wav",
        ".ogg",
        ".m4a",
        ".aac",
        ".flac",
    }
    if isinstance(value, dict):
        preferred = ("path", "filepath", "file_path", "output", "destination", "saved_path")
        for key in preferred:
            candidate = value.get(key)
            if isinstance(candidate, str) and any(candidate.casefold().endswith(suffix) for suffix in media_suffixes):
                return candidate
        for item in value.values():
            found = _find_media_path(item)
            if found:
                return found
    elif isinstance(value, list):
        for item in value:
            found = _find_media_path(item)
            if found:
                return found
    elif isinstance(value, str):
        stripped = value.strip().strip("\"'")
        if any(stripped.casefold().endswith(suffix) for suffix in media_suffixes):
            return stripped
    return None


def _result_log_details(name: str, result: Any) -> dict[str, Any]:
    """Expose bounded structured result fields for the local activity monitor."""
    if not isinstance(result, dict):
        return {}
    details: dict[str, Any] = {}
    for key in (
        "path",
        "filepath",
        "written",
        "edited",
        "dry_run",
        "size_bytes",
        "encoding",
        "newline",
        "atomic",
        "status",
        "job_id",
    ):
        if key in result:
            details[key] = redact(result[key])
    if name == "edit_file" and isinstance(result.get("diff"), str):
        details["diff"] = redact(result["diff"][:20_000])
    media_path = _find_media_path(result)
    if media_path:
        details["media_path"] = redact(media_path)
    return details


async def _warm_services(
    client: CodexAppServerClient,
    tools: CodexTools,
    logger: logging.Logger,
) -> None:
    try:
        await client.start()
        await tools.inventory.warm()
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        log_event(
            logger,
            logging.WARNING,
            "connector_warmup_failed",
            error_type=type(exc).__name__,
            error_preview=_single_line(str(exc)),
        )


async def run_server(*, acquire_lock: bool = True) -> None:
    settings = Settings.load()
    logger = configure_logging(settings)
    lock = SingleInstanceLock(settings.state_dir / "connector.lock")
    if acquire_lock:
        lock.acquire()

    client = CodexAppServerClient(
        settings.workspace,
        logger,
        request_timeout_sec=settings.default_tool_timeout_sec,
    )
    tools = CodexTools(settings, client)
    warmup_task = asyncio.create_task(
        _warm_services(client, tools, logger),
        name="codexpc-service-warmup",
    )
    server = Server("CodexPCConnector")
    active_calls: dict[str, dict[str, Any]] = {}
    call_ids = itertools.count(1)
    control_tools = [
        types.Tool(
            name="list_active_tool_calls",
            description="Lists tool calls currently running in this connector, including call IDs and elapsed time.",
            inputSchema={"type": "object", "properties": {}, "additionalProperties": False},
            annotations=types.ToolAnnotations(
                readOnlyHint=True,
                destructiveHint=False,
                idempotentHint=True,
                openWorldHint=False,
            ),
        ),
        types.Tool(
            name="cancel_tool_calls",
            description=(
                "Cancels active connector tool calls by call_id or tool name. "
                "With hard=true, stops process jobs and restarts the Codex app-server connection."
            ),
            inputSchema={
                "type": "object",
                "properties": {
                    "call_id": {"type": "string"},
                    "tool_name": {"type": "string"},
                    "hard": {"type": "boolean", "default": False},
                },
                "additionalProperties": False,
            },
            annotations=types.ToolAnnotations(
                readOnlyHint=False,
                destructiveHint=True,
                idempotentHint=True,
                openWorldHint=False,
            ),
        ),
    ]
    if settings.tool_profile != "full":
        control_tools = []

    @server.list_tools()
    async def handle_list_tools() -> list[types.Tool]:
        return tools.list_tools() + control_tools

    @server.call_tool()
    async def handle_call_tool(name: str, arguments: dict[str, Any] | None):
        safe_arguments = arguments or {}

        if name == "list_active_tool_calls":
            now = time.perf_counter()
            rows = [
                {
                    "call_id": call_id,
                    "tool": item["name"],
                    "elapsed_sec": round(now - item["started_at"], 3),
                    "argument_keys": sorted(item["arguments"]),
                }
                for call_id, item in active_calls.items()
            ]
            return [types.TextContent(type="text", text=json_text(rows, settings.max_output_chars))]

        if name == "cancel_tool_calls":
            requested_id = str(safe_arguments.get("call_id") or "").strip()
            requested_name = str(safe_arguments.get("tool_name") or "").strip()
            hard = bool(safe_arguments.get("hard", False))
            if not requested_id and not requested_name and not hard:
                return [types.TextContent(type="text", text="Error: provide call_id, tool_name, or hard=true")]
            matched: list[str] = []
            for call_id, item in list(active_calls.items()):
                if requested_id and call_id != requested_id:
                    continue
                if requested_name and item["name"] != requested_name:
                    continue
                item["task"].cancel()
                matched.append(call_id)
            remaining_active = sum(1 for call_id in active_calls if call_id not in matched)
            if hard:
                await tools.shutdown()
                await client.close()
            result = {
                "cancelled_call_ids": matched,
                "hard_reset": hard,
                "remaining_active": remaining_active,
            }
            return [types.TextContent(type="text", text=json_text(result, settings.max_output_chars))]

        call_id = f"call-{next(call_ids)}"
        started_at = time.perf_counter()
        current_task = asyncio.current_task()
        if current_task is None:
            raise RuntimeError("Tool call is not running inside an asyncio task")
        active_calls[call_id] = {
            "name": name,
            "arguments": safe_arguments,
            "started_at": started_at,
            "task": current_task,
        }
        log_event(
            logger,
            logging.INFO,
            "chatgpt_tool_call_started",
            tool=name,
            call_id=call_id,
            **_argument_summary(safe_arguments),
        )
        session_key = _session_key(server)
        try:
            result = await tools.call(name, safe_arguments, session_key=session_key, call_id=call_id)
            image_payload = result.pop("_mcp_image", None) if isinstance(result, dict) else None
            output = json_text(result, settings.max_output_chars)
            duration_ms = round((time.perf_counter() - started_at) * 1000, 1)
            log_event(
                logger,
                logging.INFO,
                "chatgpt_tool_call_succeeded",
                tool=name,
                call_id=call_id,
                duration_ms=duration_ms,
                output_chars=len(output),
                output_preview=_single_line(output),
                session_key=session_key,
                **_result_log_details(name, result),
            )
            content: list[types.TextContent | types.ImageContent] = [types.TextContent(type="text", text=output)]
            if image_payload is not None:
                content.append(
                    types.ImageContent(
                        type="image",
                        data=base64.b64encode(image_payload["data"]).decode("ascii"),
                        mimeType=image_payload["mime_type"],
                    )
                )
            return content
        except asyncio.CancelledError:
            duration_ms = round((time.perf_counter() - started_at) * 1000, 1)
            log_event(
                logger,
                logging.WARNING,
                "chatgpt_tool_call_cancelled",
                tool=name,
                call_id=call_id,
                duration_ms=duration_ms,
            )
            return [types.TextContent(type="text", text=f"Cancelled: {call_id} ({name})")]
        except Exception as exc:
            duration_ms = round((time.perf_counter() - started_at) * 1000, 1)
            log_event(
                logger,
                logging.ERROR,
                "chatgpt_tool_call_failed",
                tool=name,
                call_id=call_id,
                duration_ms=duration_ms,
                error_type=type(exc).__name__,
                error_preview=_single_line(str(exc)),
            )
            return [types.TextContent(type="text", text=f"Error: {type(exc).__name__}: {redact(str(exc))}")]
        finally:
            active_calls.pop(call_id, None)

    log_event(
        logger,
        logging.INFO,
        "connector_start",
        backend="codex_app_server",
        tool_count=len(tools.list_tools()) + len(control_tools),
    )
    try:
        async with stdio_server() as (read_stream, write_stream):
            await server.run(read_stream, write_stream, server.create_initialization_options())
    finally:
        if not warmup_task.done():
            warmup_task.cancel()
        with suppress(asyncio.CancelledError):
            await warmup_task
        await tools.shutdown()
        await client.close()
        log_event(logger, logging.INFO, "connector_stop")
        if acquire_lock:
            lock.release()
        close_logging(logger)


def _gateway_tools() -> list[types.Tool]:
    """Compatibility import for callers that inspected the old static gateway."""
    return gateway_tools()


def main() -> None:
    asyncio.run(run_server())
