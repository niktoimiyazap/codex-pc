from __future__ import annotations

import os
import sys
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


def _default_state_dir() -> Path:
    if sys.platform == "win32":
        base = Path(os.environ.get("LOCALAPPDATA", Path.home() / "AppData" / "Local"))
        return base / "CodexPCConnector"
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "CodexPCConnector"
    base = Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local" / "state"))
    return base / "codexpc-connector"


def _as_paths(value: Any, fallback: list[Path]) -> list[Path]:
    if value is None:
        return fallback
    if isinstance(value, str):
        items = [part for part in value.split(os.pathsep) if part]
    elif isinstance(value, list):
        items = [str(part) for part in value]
    else:
        return fallback
    paths = [Path(os.path.expandvars(os.path.expanduser(item))).resolve() for item in items]
    return paths or fallback


def _as_tool_profile(value: Any) -> str:
    profile = str(value or "core").strip().lower()
    if profile not in {"core", "full"}:
        raise ValueError("tool_profile must be core or full")
    return profile


@dataclass(slots=True)
class Settings:
    state_dir: Path
    workspace: Path
    allowed_roots: list[Path] = field(default_factory=list)
    default_tool_timeout_sec: float = 120.0
    max_output_chars: int = 100_000
    mcp_inventory_ttl_sec: float = 300.0
    tool_profile: str = "core"
    log_level: str = "INFO"

    @property
    def config_path(self) -> Path:
        return self.state_dir / "config.toml"

    @property
    def log_dir(self) -> Path:
        return self.state_dir / "logs"

    @classmethod
    def load(cls) -> Settings:
        state_override = os.environ.get("CODEXPC_STATE_DIR")
        state_dir = (
            Path(os.path.expandvars(os.path.expanduser(state_override))).resolve()
            if state_override
            else _default_state_dir()
        )
        state_dir.mkdir(parents=True, exist_ok=True)
        config_path = state_dir / "config.toml"
        raw: dict[str, Any] = {}
        if config_path.is_file():
            with config_path.open("rb") as handle:
                raw = tomllib.load(handle)

        home = Path.home().resolve()
        workspace = Path(
            os.path.expandvars(os.path.expanduser(os.environ.get("CODEXPC_WORKSPACE", str(raw.get("workspace", home)))))
        ).resolve()
        allowed_roots = _as_paths(
            os.environ.get("CODEXPC_ALLOWED_ROOTS", raw.get("allowed_roots")),
            [home],
        )

        return cls(
            state_dir=state_dir,
            workspace=workspace,
            allowed_roots=allowed_roots,
            default_tool_timeout_sec=float(raw.get("default_tool_timeout_sec", 120.0)),
            max_output_chars=int(raw.get("max_output_chars", 100_000)),
            mcp_inventory_ttl_sec=max(1.0, float(raw.get("mcp_inventory_ttl_sec", 300.0))),
            tool_profile=_as_tool_profile(os.environ.get("CODEXPC_TOOL_PROFILE", raw.get("tool_profile", "core"))),
            log_level=str(raw.get("log_level", "INFO")).upper(),
        )
