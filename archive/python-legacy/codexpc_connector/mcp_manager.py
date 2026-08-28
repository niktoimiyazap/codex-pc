from __future__ import annotations

import asyncio
import contextlib
import os
import shlex
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from mcp.client.session import ClientSession
from mcp.client.stdio import StdioServerParameters, stdio_client
from mcp.client.streamable_http import streamable_http_client

from .config import Settings
from .discovery import CodexMCPDiscovery, MCPServerConfig
from .logging_utils import log_event


@dataclass(slots=True)
class _Request:
    kind: str
    future: asyncio.Future[Any]
    tool_name: str | None = None
    arguments: dict[str, Any] | None = None


def _normalize_args(command: str, args: tuple[str, ...]) -> list[str]:
    if len(args) == 1 and Path(command).name.lower() in {"npx", "npx.cmd", "npx.exe"}:
        candidate = args[0]
        if candidate.startswith("-") and " " in candidate:
            return shlex.split(candidate, posix=os.name != "nt")
    return list(args)


class MCPWorker:
    def __init__(self, config: MCPServerConfig, settings: Settings, logger):
        self.config = config
        self.settings = settings
        self.logger = logger
        self._queue: asyncio.Queue[_Request] = asyncio.Queue()
        self._task: asyncio.Task[None] | None = None
        self._task_lock = asyncio.Lock()
        self._tools: list[Any] = []
        self._started_at: float | None = None
        self._last_error: str | None = None
        self._calls = 0

    @property
    def running(self) -> bool:
        return bool(self._task and not self._task.done() and self._started_at is not None)

    def status(self) -> dict[str, Any]:
        return {
            "running": self.running,
            "tool_count": len(self._tools),
            "calls": self._calls,
            "last_error": self._last_error,
            "uptime_sec": round(time.monotonic() - self._started_at, 1) if self.running and self._started_at else None,
        }

    async def _ensure_task(self) -> None:
        async with self._task_lock:
            if self._task is None or self._task.done():
                self._task = asyncio.create_task(self._run(), name=f"mcp:{self.config.name}")

    async def request(self, kind: str, *, tool_name: str | None = None, arguments: dict[str, Any] | None = None) -> Any:
        loop = asyncio.get_running_loop()
        future: asyncio.Future[Any] = loop.create_future()
        await self._ensure_task()
        await self._queue.put(_Request(kind=kind, future=future, tool_name=tool_name, arguments=arguments))
        return await future

    async def shutdown(self) -> None:
        task = self._task
        if task is None or task.done():
            return
        loop = asyncio.get_running_loop()
        future: asyncio.Future[Any] = loop.create_future()
        await self._queue.put(_Request(kind="shutdown", future=future))
        with contextlib.suppress(asyncio.TimeoutError, asyncio.CancelledError):
            await asyncio.wait_for(future, timeout=5.0)
        with contextlib.suppress(asyncio.TimeoutError, asyncio.CancelledError):
            await asyncio.wait_for(task, timeout=5.0)
        if not task.done():
            task.cancel()

    @contextlib.asynccontextmanager
    async def _transport(self):
        config = self.config
        if config.transport_type == "stdio":
            if not config.command:
                raise ValueError(f"MCP server {config.name} has no stdio command")
            merged_env = os.environ.copy()
            for env_name in config.env_vars:
                if env_name in os.environ:
                    merged_env[env_name] = os.environ[env_name]
            if config.env:
                merged_env.update(config.env)
            params = StdioServerParameters(
                command=config.command,
                args=_normalize_args(config.command, config.args),
                env=merged_env,
                cwd=config.cwd,
            )
            with open(os.devnull, "w", encoding="utf-8") as devnull:
                async with stdio_client(params, errlog=devnull) as (read_stream, write_stream):
                    yield read_stream, write_stream
            return

        if config.transport_type == "streamable_http":
            if not config.url:
                raise ValueError(f"MCP server {config.name} has no URL")
            headers = dict(config.http_headers or {})
            for header, env_name in (config.env_http_headers or {}).items():
                value = os.environ.get(env_name)
                if value:
                    headers[header] = value
            if config.bearer_token_env_var:
                token = os.environ.get(config.bearer_token_env_var)
                if token:
                    headers["Authorization"] = f"Bearer {token}"
            timeout = httpx.Timeout(
                connect=config.startup_timeout_sec or self.settings.default_startup_timeout_sec,
                read=None,
                write=config.tool_timeout_sec or self.settings.default_tool_timeout_sec,
                pool=config.startup_timeout_sec or self.settings.default_startup_timeout_sec,
            )
            async with httpx.AsyncClient(  # noqa: SIM117 - stream context depends on this client
                headers=headers,
                timeout=timeout,
                follow_redirects=True,
            ) as client:
                async with streamable_http_client(config.url, http_client=client) as (read_stream, write_stream, _):
                    yield read_stream, write_stream
            return

        raise ValueError(f"Unsupported MCP transport: {config.transport_type}")

    async def _run(self) -> None:
        startup_timeout = self.config.startup_timeout_sec or self.settings.default_startup_timeout_sec
        tool_timeout = self.config.tool_timeout_sec or self.settings.default_tool_timeout_sec
        started = time.perf_counter()
        try:
            async with self._transport() as (read_stream, write_stream):  # noqa: SIM117
                async with ClientSession(read_stream, write_stream) as session:
                    await asyncio.wait_for(session.initialize(), timeout=startup_timeout)
                    listed = await asyncio.wait_for(session.list_tools(), timeout=tool_timeout)
                    self._tools = list(listed.tools)
                    self._started_at = time.monotonic()
                    self._last_error = None
                    log_event(
                        self.logger,
                        20,
                        "mcp_started",
                        server=self.config.name,
                        transport=self.config.transport_type,
                        tool_count=len(self._tools),
                        duration_ms=round((time.perf_counter() - started) * 1000, 1),
                    )
                    while True:
                        try:
                            request = await asyncio.wait_for(
                                self._queue.get(), timeout=self.settings.mcp_idle_timeout_sec
                            )
                        except TimeoutError:
                            log_event(self.logger, 20, "mcp_idle_shutdown", server=self.config.name)
                            return

                        if request.future.cancelled():
                            continue
                        if request.kind == "shutdown":
                            request.future.set_result(True)
                            return
                        if request.kind == "list":
                            request.future.set_result(list(self._tools))
                            continue
                        if request.kind == "call":
                            if not request.tool_name:
                                request.future.set_exception(ValueError("tool_name is required"))
                                continue
                            call_started = time.perf_counter()
                            try:
                                result = await asyncio.wait_for(
                                    session.call_tool(request.tool_name, request.arguments or {}),
                                    timeout=tool_timeout,
                                )
                                self._calls += 1
                                request.future.set_result(result)
                                log_event(
                                    self.logger,
                                    20,
                                    "mcp_call",
                                    server=self.config.name,
                                    tool=request.tool_name,
                                    duration_ms=round((time.perf_counter() - call_started) * 1000, 1),
                                    ok=True,
                                )
                            except Exception as exc:
                                self._last_error = f"{type(exc).__name__}: {exc}"
                                request.future.set_exception(exc)
                                log_event(
                                    self.logger,
                                    40,
                                    "mcp_call",
                                    server=self.config.name,
                                    tool=request.tool_name,
                                    duration_ms=round((time.perf_counter() - call_started) * 1000, 1),
                                    ok=False,
                                    error_type=type(exc).__name__,
                                )
                            continue
                        request.future.set_exception(ValueError(f"Unknown worker request: {request.kind}"))
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self._last_error = f"{type(exc).__name__}: {exc}"
            log_event(
                self.logger,
                40,
                "mcp_start_failed",
                server=self.config.name,
                transport=self.config.transport_type,
                error_type=type(exc).__name__,
                duration_ms=round((time.perf_counter() - started) * 1000, 1),
            )
            while not self._queue.empty():
                request = self._queue.get_nowait()
                if not request.future.done():
                    request.future.set_exception(exc)
        finally:
            self._started_at = None


class MCPManager:
    def __init__(self, discovery: CodexMCPDiscovery, settings: Settings, logger):
        self.discovery = discovery
        self.settings = settings
        self.logger = logger
        self._workers: dict[str, MCPWorker] = {}
        self._lock = asyncio.Lock()

    async def _get_worker(self, name: str) -> MCPWorker:
        servers = await self.discovery.get_servers()
        config = servers.get(name)
        if config is None:
            raise KeyError(f"Unknown Codex MCP server: {name}")
        if not config.enabled:
            raise PermissionError(f"MCP server is disabled: {name}")
        async with self._lock:
            current = self._workers.get(name)
            if current and current.config.fingerprint != config.fingerprint:
                await current.shutdown()
                current = None
            if current is None:
                current = MCPWorker(config, self.settings, self.logger)
                self._workers[name] = current
            return current

    async def list_servers(self, *, refresh: bool = False) -> list[dict[str, Any]]:
        servers = await self.discovery.get_servers(force=refresh)
        result = []
        for name in sorted(servers):
            summary = servers[name].public_summary()
            worker = self._workers.get(name)
            summary["runtime"] = worker.status() if worker else {
                "running": False,
                "tool_count": 0,
                "calls": 0,
                "last_error": None,
                "uptime_sec": None,
            }
            result.append(summary)
        return result

    async def list_tools(self, server_name: str) -> list[Any]:
        worker = await self._get_worker(server_name)
        return await worker.request("list")

    async def search_tools(
        self,
        query: str,
        *,
        server_name: str | None = None,
        max_results: int = 20,
    ) -> list[dict[str, Any]]:
        query_folded = query.casefold().strip()
        if not query_folded:
            raise ValueError("query must not be empty")
        servers = await self.discovery.get_servers()
        names = [server_name] if server_name else [name for name, config in servers.items() if config.enabled]
        results: list[dict[str, Any]] = []

        async def collect(name: str) -> None:
            try:
                tools = await self.list_tools(name)
            except Exception as exc:
                results.append({"server": name, "error": f"{type(exc).__name__}: {exc}"})
                return
            for tool in tools:
                haystack = f"{tool.name} {tool.description or ''}".casefold()
                if query_folded in haystack:
                    results.append({
                        "server": name,
                        "name": tool.name,
                        "description": tool.description,
                        "inputSchema": tool.inputSchema,
                    })

        await asyncio.gather(*(collect(name) for name in names))
        matches = [item for item in results if "name" in item]
        errors = [item for item in results if "error" in item]
        return matches[:max_results] + errors[: max(0, max_results - len(matches[:max_results]))]

    async def call_tool(self, server_name: str, tool_name: str, arguments: dict[str, Any]) -> Any:
        worker = await self._get_worker(server_name)
        return await worker.request("call", tool_name=tool_name, arguments=arguments)

    async def shutdown(self) -> None:
        workers = list(self._workers.values())
        await asyncio.gather(*(worker.shutdown() for worker in workers), return_exceptions=True)
