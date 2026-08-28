from __future__ import annotations

import asyncio
import json
import os
import tempfile
import time
from contextlib import suppress
from pathlib import Path
from typing import Any

from .app_server import CodexAppServerClient

_CACHE_VERSION = 1
_INVENTORY_SESSION_KEY = "__codexpc_inventory__"


def _normalize_tool(tool_name: str, value: Any) -> dict[str, Any]:
    payload = value if isinstance(value, dict) else {}
    input_schema = payload.get("inputSchema")
    if not isinstance(input_schema, dict):
        input_schema = {"type": "object"}
    result: dict[str, Any] = {
        "name": str(payload.get("name") or tool_name),
        "description": str(payload.get("description") or ""),
        "inputSchema": input_schema,
    }
    annotations = payload.get("annotations")
    if isinstance(annotations, dict):
        result["annotations"] = annotations
    return result


def _normalize_server(value: Any) -> dict[str, Any] | None:
    if not isinstance(value, dict):
        return None
    name = str(value.get("name") or "").strip()
    if not name:
        return None

    raw_tools = value.get("tools")
    tools: dict[str, dict[str, Any]] = {}
    if isinstance(raw_tools, dict):
        for tool_name, tool in raw_tools.items():
            normalized_name = str(tool_name).strip()
            if normalized_name:
                tools[normalized_name] = _normalize_tool(normalized_name, tool)

    return {
        "name": name,
        "authStatus": value.get("authStatus"),
        "enabled": bool(value.get("enabled", True)),
        "disabledReason": value.get("disabledReason"),
        "transport": value.get("transport"),
        "tools": tools,
    }


def _read_cache(path: Path) -> tuple[float, list[dict[str, Any]]] | None:
    if not path.is_file():
        return None
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    if payload.get("version") != _CACHE_VERSION:
        return None
    updated_at = payload.get("updated_at")
    raw_servers = payload.get("servers")
    if not isinstance(updated_at, (int, float)) or not isinstance(raw_servers, list):
        return None
    servers = [server for item in raw_servers if (server := _normalize_server(item)) is not None]
    return float(updated_at), servers


def _write_cache(path: Path, updated_at: float, servers: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(
        {"version": _CACHE_VERSION, "updated_at": updated_at, "servers": servers},
        ensure_ascii=False,
        separators=(",", ":"),
    ).encode("utf-8")
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if temporary.exists():
            temporary.unlink()


class McpInventoryService:
    """Shared downstream MCP inventory with persistent stale-while-revalidate caching."""

    def __init__(
        self,
        client: CodexAppServerClient,
        cache_path: Path,
        *,
        ttl_sec: float = 300.0,
    ) -> None:
        self.client = client
        self.cache_path = cache_path
        self.ttl_sec = max(1.0, float(ttl_sec))
        self._servers: list[dict[str, Any]] = []
        self._updated_at = 0.0
        self._has_inventory = False
        self._loaded = False
        self._load_lock = asyncio.Lock()
        self._refresh_lock = asyncio.Lock()
        self._refresh_task: asyncio.Task[None] | None = None
        self._last_error: str | None = None

    async def shutdown(self) -> None:
        task = self._refresh_task
        if task and not task.done():
            task.cancel()
            with suppress(asyncio.CancelledError):
                await task
        self._refresh_task = None

    async def warm(self) -> None:
        """Load the persistent cache and refresh stale data without blocking startup."""
        await self._ensure_loaded()
        if not self._has_inventory or self._age_sec() > self.ttl_sec:
            self._schedule_refresh()

    async def get(self, *, refresh: bool = False) -> dict[str, Any]:
        await self._ensure_loaded()
        if refresh:
            await self._refresh(force=True)
        elif not self._has_inventory:
            await self._refresh()
        elif self._age_sec() > self.ttl_sec:
            self._schedule_refresh()
        return self._snapshot()

    async def discover(
        self,
        *,
        query: str = "",
        server_name: str | None = None,
        limit: int = 50,
        refresh: bool = False,
    ) -> dict[str, Any]:
        snapshot = await self.get(refresh=refresh)
        normalized_server = (server_name or "").strip()
        selected = [
            server for server in snapshot["servers"] if not normalized_server or server.get("name") == normalized_server
        ]
        if normalized_server and not selected:
            raise KeyError(f"Unknown Codex MCP server: {normalized_server}")

        needle = query.strip().casefold()
        tools: list[dict[str, Any]] = []
        for server in selected:
            for tool_name, tool in server.get("tools", {}).items():
                searchable = f"{tool_name} {tool.get('description', '')}".casefold()
                if needle and needle not in searchable:
                    continue
                tools.append({"server": server["name"], "name": tool_name, **tool})
                if len(tools) >= limit:
                    break
            if len(tools) >= limit:
                break

        return {
            **snapshot,
            "servers": [self._server_summary(server) for server in selected],
            "tools": tools,
            "query": query,
            "server_name": normalized_server or None,
            "limit": limit,
            "truncated": len(tools) >= limit,
        }

    async def list_servers(self, *, refresh: bool = False) -> list[dict[str, Any]]:
        snapshot = await self.get(refresh=refresh)
        return [self._server_summary(server) for server in snapshot["servers"]]

    async def list_tools(self, server_name: str, *, refresh: bool = False) -> list[dict[str, Any]]:
        result = await self.discover(server_name=server_name, limit=10_000, refresh=refresh)
        return result["tools"]

    async def search_tools(
        self,
        query: str,
        *,
        server_name: str | None = None,
        limit: int = 100,
        refresh: bool = False,
    ) -> list[dict[str, Any]]:
        result = await self.discover(
            query=query,
            server_name=server_name,
            limit=limit,
            refresh=refresh,
        )
        return result["tools"]

    async def _ensure_loaded(self) -> None:
        if self._loaded:
            return
        async with self._load_lock:
            if self._loaded:
                return
            cached = await asyncio.to_thread(_read_cache, self.cache_path)
            if cached is not None:
                self._updated_at, self._servers = cached
                self._has_inventory = True
            self._loaded = True

    async def _refresh(self, *, force: bool = False) -> None:
        async with self._refresh_lock:
            if not force and self._has_inventory and self._age_sec() <= self.ttl_sec:
                return
            thread_id = await self.client.ensure_thread(_INVENTORY_SESSION_KEY)
            cursor: str | None = None
            raw_servers: list[Any] = []
            while True:
                result = await self.client.request(
                    "mcpServerStatus/list",
                    {"threadId": thread_id, "detail": "toolsAndAuthOnly", "cursor": cursor, "limit": 100},
                )
                raw_servers.extend(result.get("data", []))
                cursor = result.get("nextCursor")
                if not cursor:
                    break

            servers = [server for item in raw_servers if (server := _normalize_server(item)) is not None]
            updated_at = time.time()
            self._servers = servers
            self._updated_at = updated_at
            self._has_inventory = True
            self._last_error = None
            _write_cache(self.cache_path, updated_at, servers)

    def _schedule_refresh(self) -> None:
        if self._refresh_task and not self._refresh_task.done():
            return
        self._refresh_task = asyncio.create_task(
            self._refresh_in_background(),
            name="codexpc-mcp-inventory-refresh",
        )

    async def _refresh_in_background(self) -> None:
        try:
            await self._refresh()
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self._last_error = f"{type(exc).__name__}: {exc}"

    def _age_sec(self) -> float:
        if self._updated_at <= 0:
            return float("inf")
        return max(0.0, time.time() - self._updated_at)

    def _snapshot(self) -> dict[str, Any]:
        age_sec = self._age_sec()
        return {
            "servers": list(self._servers),
            "updated_at": self._updated_at or None,
            "age_sec": None if age_sec == float("inf") else round(age_sec, 3),
            "cached": self._has_inventory,
            "stale": self._has_inventory and age_sec > self.ttl_sec,
            "refreshing": bool(self._refresh_task and not self._refresh_task.done()),
            "last_error": self._last_error,
        }

    @staticmethod
    def _server_summary(server: dict[str, Any]) -> dict[str, Any]:
        return {
            "name": server["name"],
            "authStatus": server.get("authStatus"),
            "toolCount": len(server.get("tools", {})),
            "enabled": bool(server.get("enabled", True)),
            "disabledReason": server.get("disabledReason"),
            "transport": server.get("transport"),
        }
