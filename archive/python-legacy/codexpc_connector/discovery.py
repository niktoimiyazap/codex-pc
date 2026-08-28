from __future__ import annotations

import asyncio
import hashlib
import json
import os
import shutil
import subprocess  # nosec B404
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit, urlunsplit

from .config import Settings
from .logging_utils import log_event
from .security import redact, safe_env_summary


@dataclass(slots=True, frozen=True)
class MCPServerConfig:
    name: str
    enabled: bool
    transport_type: str
    command: str | None = None
    args: tuple[str, ...] = ()
    env: dict[str, str] | None = None
    env_vars: tuple[str, ...] = ()
    cwd: str | None = None
    url: str | None = None
    bearer_token_env_var: str | None = None
    http_headers: dict[str, str] | None = None
    env_http_headers: dict[str, str] | None = None
    startup_timeout_sec: float | None = None
    tool_timeout_sec: float | None = None
    disabled_reason: str | None = None

    @property
    def fingerprint(self) -> str:
        payload = {
            "name": self.name,
            "enabled": self.enabled,
            "transport_type": self.transport_type,
            "command": self.command,
            "args": self.args,
            "env": self.env,
            "env_vars": self.env_vars,
            "cwd": self.cwd,
            "url": self.url,
            "bearer_token_env_var": self.bearer_token_env_var,
            "http_headers": self.http_headers,
            "env_http_headers": self.env_http_headers,
            "startup_timeout_sec": self.startup_timeout_sec,
            "tool_timeout_sec": self.tool_timeout_sec,
        }
        raw = json.dumps(payload, sort_keys=True, ensure_ascii=False).encode("utf-8")
        return hashlib.sha256(raw).hexdigest()

    def public_summary(self) -> dict[str, Any]:
        safe_url = None
        if self.url:
            parsed = urlsplit(self.url)
            hostname = parsed.hostname or ""
            port = f":{parsed.port}" if parsed.port else ""
            safe_url = urlunsplit((parsed.scheme, f"{hostname}{port}", parsed.path, "", ""))
        return {
            "name": self.name,
            "enabled": self.enabled,
            "disabled_reason": self.disabled_reason,
            "transport": self.transport_type,
            "command": self.command,
            "arg_count": len(self.args),
            "cwd": self.cwd,
            "url": safe_url,
            "env_keys": safe_env_summary(self.env),
            "env_vars": list(self.env_vars),
            "http_header_keys": sorted((self.http_headers or {}).keys()),
            "env_http_header_keys": sorted((self.env_http_headers or {}).keys()),
            "bearer_token_env_var": self.bearer_token_env_var,
            "startup_timeout_sec": self.startup_timeout_sec,
            "tool_timeout_sec": self.tool_timeout_sec,
        }


class CodexMCPDiscovery:
    def __init__(self, settings: Settings, logger):
        self.settings = settings
        self.logger = logger
        self._cache: dict[str, MCPServerConfig] = {}
        self._cache_at = 0.0
        self._lock = asyncio.Lock()

    @staticmethod
    def _codex_command() -> str:
        command = shutil.which("codex")
        if not command:
            raise FileNotFoundError("Codex CLI was not found in PATH")
        return command

    async def _run_codex_json(self) -> list[dict[str, Any]]:
        command = self._codex_command()
        argv = [command, "mcp", "list", "--json"]
        if os.name == "nt" and command.lower().endswith((".cmd", ".bat")):
            comspec = os.environ.get("COMSPEC", "cmd.exe")
            line = subprocess.list2cmdline(argv)
            process = await asyncio.create_subprocess_exec(
                comspec,
                "/d",
                "/s",
                "/c",
                line,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.DEVNULL,
            )
        else:
            process = await asyncio.create_subprocess_exec(
                *argv,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.DEVNULL,
            )
        try:
            stdout, stderr = await asyncio.wait_for(process.communicate(), timeout=20.0)
        except TimeoutError:
            process.kill()
            await process.wait()
            raise TimeoutError("Timed out while reading MCP configuration from Codex") from None
        if process.returncode != 0:
            message = redact(stderr.decode("utf-8", errors="replace")[:2000])
            raise RuntimeError(f"Codex MCP discovery failed with exit code {process.returncode}: {message}")
        payload = json.loads(stdout.decode("utf-8-sig"))
        if not isinstance(payload, list):
            raise ValueError("Codex MCP list returned a non-list JSON payload")
        return payload

    @staticmethod
    def _parse(item: dict[str, Any]) -> MCPServerConfig:
        transport = item.get("transport") or {}
        transport_type = str(transport.get("type", "unknown"))
        env = transport.get("env")
        if env is not None and not isinstance(env, dict):
            env = None
        return MCPServerConfig(
            name=str(item.get("name", "")),
            enabled=bool(item.get("enabled", False)),
            disabled_reason=item.get("disabled_reason"),
            transport_type=transport_type,
            command=transport.get("command"),
            args=tuple(str(arg) for arg in (transport.get("args") or [])),
            env={str(k): str(v) for k, v in (env or {}).items()} or None,
            env_vars=tuple(str(v) for v in (transport.get("env_vars") or [])),
            cwd=transport.get("cwd"),
            url=transport.get("url"),
            bearer_token_env_var=transport.get("bearer_token_env_var"),
            http_headers={str(k): str(v) for k, v in (transport.get("http_headers") or {}).items()} or None,
            env_http_headers={str(k): str(v) for k, v in (transport.get("env_http_headers") or {}).items()} or None,
            startup_timeout_sec=item.get("startup_timeout_sec"),
            tool_timeout_sec=item.get("tool_timeout_sec"),
        )

    async def get_servers(self, *, force: bool = False) -> dict[str, MCPServerConfig]:
        now = time.monotonic()
        if not force and self._cache and now - self._cache_at < self.settings.discovery_ttl_sec:
            return dict(self._cache)
        async with self._lock:
            now = time.monotonic()
            if not force and self._cache and now - self._cache_at < self.settings.discovery_ttl_sec:
                return dict(self._cache)
            started = time.perf_counter()
            try:
                items = await self._run_codex_json()
            except Exception as exc:
                if self._cache:
                    log_event(
                        self.logger,
                        30,
                        "codex_mcp_discovery_stale_cache",
                        server_count=len(self._cache),
                        duration_ms=round((time.perf_counter() - started) * 1000, 1),
                        error_type=type(exc).__name__,
                    )
                    return dict(self._cache)
                raise
            parsed = {
                config.name: config
                for config in (self._parse(item) for item in items)
                if config.name
            }
            self._cache = parsed
            self._cache_at = time.monotonic()
            log_event(
                self.logger,
                20,
                "codex_mcp_discovery",
                server_count=len(parsed),
                duration_ms=round((time.perf_counter() - started) * 1000, 1),
                server_names=sorted(parsed),
            )
            return dict(parsed)
