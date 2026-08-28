from __future__ import annotations

import difflib
import hashlib
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class FileEditError(ValueError):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


@dataclass(slots=True)
class TextSnapshot:
    path: Path
    data: bytes
    text: str
    encoding: str
    newline: str
    final_newline: bool
    sha256: str


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def detect_newline(text: str) -> str:
    crlf = text.count("\r\n")
    lf = text.count("\n") - crlf
    cr = text.count("\r") - crlf
    if crlf >= lf and crlf >= cr and crlf:
        return "crlf"
    if lf >= cr and lf:
        return "lf"
    if cr:
        return "cr"
    return "none"


def read_snapshot(path: Path, *, encoding: str = "utf-8") -> TextSnapshot:
    data = path.read_bytes()
    if b"\x00" in data and encoding not in {"utf-16-le", "utf-8-sig"}:
        raise FileEditError("BINARY_FILE", "File contains NUL bytes and is not safe for text editing")
    try:
        text = data.decode(encoding)
    except UnicodeDecodeError as exc:
        raise FileEditError("UNSUPPORTED_ENCODING", f"File cannot be decoded as {encoding}") from exc
    return TextSnapshot(
        path=path,
        data=data,
        text=text,
        encoding=encoding,
        newline=detect_newline(text),
        final_newline=text.endswith(("\n", "\r")),
        sha256=sha256_bytes(data),
    )


def apply_exact_edits(text: str, edits: list[dict[str, Any]]) -> tuple[str, int]:
    result = text
    replacements = 0
    for index, edit in enumerate(edits):
        old_text = str(edit.get("old_text", ""))
        new_text = str(edit.get("new_text", ""))
        expected_count = int(edit.get("expected_count", 1))
        replace_all = bool(edit.get("replace_all", False))
        if not old_text:
            raise FileEditError("INVALID_EDIT", f"Edit {index} has empty old_text")
        actual_count = result.count(old_text)
        if replace_all:
            if actual_count == 0:
                raise FileEditError("MATCH_NOT_FOUND", f"Edit {index} did not match")
            result = result.replace(old_text, new_text)
            replacements += actual_count
            continue
        if actual_count == 0:
            raise FileEditError("MATCH_NOT_FOUND", f"Edit {index} did not match")
        if actual_count != expected_count:
            raise FileEditError(
                "AMBIGUOUS_MATCH",
                f"Edit {index} expected {expected_count} match(es), found {actual_count}",
            )
        result = result.replace(old_text, new_text, expected_count)
        replacements += expected_count
    return result, replacements


def unified_diff(path: Path, before: str, after: str) -> str:
    return "".join(
        difflib.unified_diff(
            before.splitlines(keepends=True),
            after.splitlines(keepends=True),
            fromfile=str(path),
            tofile=str(path),
        )
    )


def atomic_write(path: Path, data: bytes, *, expected_sha256: str | None = None) -> None:
    if expected_sha256 is not None:
        current = path.read_bytes()
        if sha256_bytes(current) != expected_sha256:
            raise FileEditError("STALE_FILE", "File changed after it was read")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        if expected_sha256 is not None:
            current = path.read_bytes()
            if sha256_bytes(current) != expected_sha256:
                raise FileEditError("STALE_FILE", "File changed before atomic replacement")
        os.replace(temp_path, path)
    finally:
        temp_path.unlink(missing_ok=True)
