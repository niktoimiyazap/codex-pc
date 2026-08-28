from __future__ import annotations

import os
import re
from pathlib import Path
from typing import Any

from .config import Settings

_SECRET_KEY = re.compile(
    r"(?:authorization|bearer|cookie|password|passwd|secret|token|api[_-]?key|api[_-]?hash|session)",
    re.IGNORECASE,
)
_SECRET_VALUE_PATTERNS = [
    re.compile(r"github_pat_[A-Za-z0-9_]{20,}"),
    re.compile(r"gh[pousr]_[A-Za-z0-9]{20,}"),
    re.compile(r"sk-[A-Za-z0-9_-]{20,}"),
    re.compile(r"(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}=*"),
]


def redact(value: Any, key: str | None = None) -> Any:
    if key and _SECRET_KEY.search(key):
        return "***"
    if isinstance(value, dict):
        return {str(k): redact(v, str(k)) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(item) for item in value]
    if isinstance(value, tuple):
        return tuple(redact(item) for item in value)
    if isinstance(value, str):
        result = value
        for pattern in _SECRET_VALUE_PATTERNS:
            result = pattern.sub("***", result)
        return result
    return value


def safe_env_summary(env: dict[str, str] | None) -> list[str]:
    return sorted((env or {}).keys())


class PathPolicy:
    def __init__(self, settings: Settings):
        self.settings = settings
        self.allowed_roots = [root.resolve() for root in settings.allowed_roots]
        self.denied_write_roots = self._system_roots()

    @staticmethod
    def _system_roots() -> list[Path]:
        roots: list[Path] = []
        if os.name == "nt":
            for name, fallback in (
                ("WINDIR", r"C:\Windows"),
                ("ProgramFiles", r"C:\Program Files"),
                ("ProgramFiles(x86)", r"C:\Program Files (x86)"),
                ("ProgramData", r"C:\ProgramData"),
            ):
                raw = os.environ.get(name, fallback)
                roots.append(Path(raw).resolve())
        else:
            system_roots = (
                "/bin",
                "/boot",
                "/dev",
                "/etc",
                "/lib",
                "/proc",
                "/root",
                "/sbin",
                "/sys",
                "/usr",
            )
            roots.extend(Path(item) for item in system_roots)
        return roots

    @staticmethod
    def _within(path: Path, root: Path) -> bool:
        try:
            normalized_path = os.path.normcase(os.path.abspath(path))
            normalized_root = os.path.normcase(os.path.abspath(root))
            return os.path.commonpath([normalized_path, normalized_root]) == normalized_root
        except ValueError:
            return False

    def resolve(self, raw_path: str | os.PathLike[str], *, base: Path | None = None) -> Path:
        path = Path(os.path.expandvars(os.path.expanduser(str(raw_path))))
        if not path.is_absolute():
            path = (base or self.settings.workspace) / path
        resolved = path.resolve(strict=False)
        if not any(self._within(resolved, root) for root in self.allowed_roots):
            roots = ", ".join(str(root) for root in self.allowed_roots)
            raise PermissionError(f"Path is outside allowed roots ({roots}): {resolved}")
        return resolved

    def ensure_writable(self, path: Path, *, confirm_sensitive: bool = False) -> None:
        resolved = path.resolve(strict=False)
        if not confirm_sensitive and any(self._within(resolved, root) for root in self.denied_write_roots):
            raise PermissionError(f"Writing to a protected system path requires explicit confirmation: {resolved}")

    def ensure_file(self, path: Path) -> None:
        if not path.is_file():
            raise FileNotFoundError(f"File does not exist: {path}")

    def ensure_directory(self, path: Path) -> None:
        if not path.is_dir():
            raise FileNotFoundError(f"Directory does not exist: {path}")
