import os
import asyncio
import mcp.types as types
import uuid
import difflib
import hashlib
import tempfile
from itertools import islice
from pathlib import Path

CURRENT_WORKING_DIR = os.path.expanduser("~")
BACKGROUND_TASKS = {}

def resolve_path(path: str) -> str:
    if not path:
        return CURRENT_WORKING_DIR
    if os.path.isabs(path):
        return path
    return os.path.normpath(os.path.join(CURRENT_WORKING_DIR, path))


SEARCH_SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv", "venv"}
SEARCH_MAX_FILE_BYTES = 5 * 1024 * 1024
COMMAND_OUTPUT_LIMIT = 200_000


def _truncate_output(value: str) -> str:
    if len(value) <= COMMAND_OUTPUT_LIMIT:
        return value
    return value[:COMMAND_OUTPUT_LIMIT] + "\n... output truncated ...\n"


def _normalize_newlines(value: str) -> str:
    return value.replace("\r\n", "\n").replace("\r", "\n")


def _detect_newline(text: str) -> str:
    crlf = text.count("\r\n")
    lf = text.count("\n") - crlf
    cr = text.count("\r") - crlf
    if crlf >= lf and crlf >= cr and crlf:
        return "\r\n"
    if cr > lf and cr:
        return "\r"
    return "\n"


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _atomic_write_utf8(filepath: str, content: str) -> bytes:
    encoded = content.encode("utf-8")
    directory = os.path.dirname(os.path.abspath(filepath))
    os.makedirs(directory, exist_ok=True)
    original_mode = os.stat(filepath).st_mode if os.path.exists(filepath) else None
    temp_path = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb",
            delete=False,
            dir=directory,
            prefix=".write-",
            suffix=".tmp",
        ) as temp:
            temp.write(encoded)
            temp.flush()
            os.fsync(temp.fileno())
            temp_path = temp.name
        if original_mode is not None:
            os.chmod(temp_path, original_mode)
        os.replace(temp_path, filepath)
    finally:
        if temp_path and os.path.exists(temp_path):
            os.remove(temp_path)
    return encoded


def _is_sensitive_path(filepath: str) -> bool:
    normalized = os.path.normcase(os.path.abspath(filepath))
    sensitive_roots = [
        os.path.normcase(os.path.abspath(os.environ.get("WINDIR", r"C:\Windows"))),
        os.path.normcase(os.path.abspath(os.environ.get("ProgramFiles", r"C:\Program Files"))),
        os.path.normcase(os.path.abspath(os.environ.get("ProgramFiles(x86)", r"C:\Program Files (x86)"))),
    ]
    for root in sensitive_roots:
        try:
            if os.path.commonpath([normalized, root]) == root:
                return True
        except ValueError:
            continue
    return False


def _format_numbered_lines(lines: list[str], first_line: int) -> str:
    last_line = first_line + len(lines) - 1
    width = max(1, len(str(max(first_line, last_line))))
    return "".join(f"{number:>{width}} | {line}" for number, line in enumerate(lines, start=first_line))


def _content_as_lines(content: str, newline: str, terminate_last: bool) -> list[str]:
    normalized = _normalize_newlines(content)
    if normalized == "":
        return []
    raw_lines = normalized.split("\n")
    if normalized.endswith("\n"):
        raw_lines = raw_lines[:-1]
        terminate_last = True
    result = []
    for index, line in enumerate(raw_lines):
        is_last = index == len(raw_lines) - 1
        result.append(line + (newline if terminate_last or not is_last else ""))
    return result


def _patch_file_sync(filepath: str, operations: list[dict], expected_sha256: str | None,
                     dry_run: bool, context_lines: int, confirm: bool) -> str:
    if not os.path.isfile(filepath):
        return f"Error: File does not exist: {filepath}"
    if _is_sensitive_path(filepath) and not confirm:
        return f"WARNING: Sensitive system path. Call again with confirm=true to patch: {filepath}"
    if not operations:
        return "Error: operations must not be empty"

    original_bytes = Path(filepath).read_bytes()
    original_hash = _sha256_bytes(original_bytes)
    if expected_sha256 and original_hash.casefold() != expected_sha256.casefold():
        return f"Error: File changed since it was read. Expected SHA-256 {expected_sha256}, current {original_hash}"

    has_bom = original_bytes.startswith(b"\xef\xbb\xbf")
    try:
        original_text = original_bytes.decode("utf-8-sig")
    except UnicodeDecodeError:
        return "Error: File is not valid UTF-8 text"

    newline = _detect_newline(original_text)
    original_lines = original_text.splitlines(keepends=True)
    total_lines = len(original_lines)
    file_has_final_newline = original_text.endswith(("\n", "\r"))

    prepared = []
    occupied_ranges = []
    insertion_anchors = set()
    valid_actions = {"replace", "delete", "insert_before", "insert_after"}

    for index, operation in enumerate(operations, start=1):
        action = operation.get("action")
        if action not in valid_actions:
            return f"Error: Operation {index} has invalid action '{action}'"
        if action != "delete" and "content" not in operation:
            return f"Error: Operation {index} action '{action}' requires content"
        try:
            start_line = int(operation.get("start_line"))
        except (TypeError, ValueError):
            return f"Error: Operation {index} requires start_line"
        end_line = int(operation.get("end_line", start_line))
        max_line = max(1, total_lines)
        if start_line < 1 or start_line > max_line:
            return f"Error: Operation {index} start_line {start_line} is outside 1..{max_line}"
        if end_line < start_line or end_line > max_line:
            return f"Error: Operation {index} end_line {end_line} is outside {start_line}..{max_line}"
        if total_lines == 0 and action not in {"insert_before", "insert_after"}:
            return f"Error: Operation {index} cannot {action} an empty file"

        if action in {"replace", "delete"}:
            for used_start, used_end in occupied_ranges:
                if not (end_line < used_start or start_line > used_end):
                    return f"Error: Operation {index} overlaps another replace/delete range"
            occupied_ranges.append((start_line, end_line))
        else:
            anchor = (action, start_line)
            if anchor in insertion_anchors:
                return f"Error: Operation {index} duplicates insertion anchor {action} line {start_line}"
            insertion_anchors.add(anchor)

        expected = operation.get("expected")
        if expected is not None:
            if total_lines == 0:
                actual = ""
            elif action in {"replace", "delete"}:
                actual = "".join(original_lines[start_line - 1:end_line])
            else:
                actual = original_lines[start_line - 1]
            if _normalize_newlines(actual).rstrip("\n") != _normalize_newlines(str(expected)).rstrip("\n"):
                return f"Error: Operation {index} context mismatch at lines {start_line}-{end_line}; re-read the file before patching"

        prepared.append({
            "index": index,
            "action": action,
            "start": start_line,
            "end": end_line,
            "content": str(operation.get("content", "")),
        })

    for operation in prepared:
        if operation["action"] not in {"insert_before", "insert_after"}:
            continue
        anchor = operation["start"]
        for range_start, range_end in occupied_ranges:
            conflicts = (
                operation["action"] == "insert_before" and range_start < anchor <= range_end
            ) or (
                operation["action"] == "insert_after" and range_start <= anchor < range_end
            )
            if conflicts:
                return f"Error: Operation {operation['index']} inserts inside a replace/delete range"

    # Coordinates always refer to the original file. Descending application keeps them stable.
    def sort_key(op: dict):
        action_order = {"insert_after": 3, "replace": 2, "delete": 2, "insert_before": 1}
        return (op["start"], action_order[op["action"]], op["index"])

    updated_lines = list(original_lines)
    for operation in sorted(prepared, key=sort_key, reverse=True):
        action = operation["action"]
        start = operation["start"]
        end = operation["end"]
        content = operation["content"]

        if action == "delete":
            del updated_lines[start - 1:end]
            continue

        if action == "replace":
            terminate = end < total_lines or file_has_final_newline
            replacement = _content_as_lines(content, newline, terminate)
            updated_lines[start - 1:end] = replacement
            continue

        if action == "insert_before":
            inserted = _content_as_lines(content, newline, True if total_lines else file_has_final_newline)
            updated_lines[start - 1:start - 1] = inserted
            continue

        terminate = start < total_lines or file_has_final_newline
        inserted = _content_as_lines(content, newline, terminate)
        updated_lines[start:start] = inserted

    updated_text = "".join(updated_lines)
    updated_bytes = ((b"\xef\xbb\xbf" if has_bom else b"") + updated_text.encode("utf-8"))
    updated_hash = _sha256_bytes(updated_bytes)

    diff = "".join(difflib.unified_diff(
        original_text.splitlines(keepends=True),
        updated_text.splitlines(keepends=True),
        fromfile=filepath,
        tofile=filepath,
        n=context_lines,
    ))
    if not diff:
        return f"No changes. SHA-256: {original_hash}"
    if len(diff) > 30000:
        diff = diff[:30000] + "\n... diff truncated at 30000 characters ...\n"

    if dry_run:
        return f"DRY RUN: {len(prepared)} operation(s), file not changed\nOld SHA-256: {original_hash}\nNew SHA-256: {updated_hash}\n\n{diff}"

    directory = os.path.dirname(os.path.abspath(filepath))
    original_mode = os.stat(filepath).st_mode
    temp_path = None
    try:
        with tempfile.NamedTemporaryFile(mode="wb", delete=False, dir=directory, prefix=".patch-", suffix=".tmp") as temp:
            temp.write(updated_bytes)
            temp.flush()
            os.fsync(temp.fileno())
            temp_path = temp.name
        os.chmod(temp_path, original_mode)
        os.replace(temp_path, filepath)
    finally:
        if temp_path and os.path.exists(temp_path):
            os.remove(temp_path)

    return f"Patched {filepath}: {len(prepared)} operation(s)\nOld SHA-256: {original_hash}\nNew SHA-256: {updated_hash}\n\n{diff}"


def _search_files_sync(directory: str, keyword: str, max_results: int) -> list[str]:
    needle = keyword.casefold()
    matches = []
    for root, dirs, files in os.walk(directory):
        dirs[:] = [name for name in dirs if name not in SEARCH_SKIP_DIRS]
        for filename in files:
            filepath = os.path.join(root, filename)
            try:
                if os.path.getsize(filepath) > SEARCH_MAX_FILE_BYTES:
                    continue
                with open(filepath, "r", encoding="utf-8", errors="ignore") as handle:
                    for line_number, line in enumerate(handle, start=1):
                        if needle in line.casefold():
                            preview = line.rstrip().replace("\x00", "")[:300]
                            matches.append(f"{filepath}:{line_number}: {preview}")
                            if len(matches) >= max_results:
                                return matches
            except (OSError, UnicodeError):
                continue
    return matches

def get_local_tools():
    return [
        types.Tool(
            name="set_working_directory",
            description="Sets the current working directory for all other tools. Use this to switch projects.",
            inputSchema={
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Path to set as working directory"},
                    "confirm": {"type": "boolean", "description": "Set to true to confirm sensitive paths"}
                },
                "required": ["path"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="read_file",
            description="Reads selected UTF-8 file context. Supports line ranges, context around one line, line numbers, output limits, and SHA-256 for safe patching.",
            inputSchema={
                "type": "object",
                "properties": {
                    "filepath": {"type": "string", "description": "Path to the file"},
                    "start_line": {"type": "integer", "minimum": 1, "description": "Optional first line, 1-based"},
                    "end_line": {"type": "integer", "minimum": 1, "description": "Optional last line, inclusive"},
                    "around_line": {"type": "integer", "minimum": 1, "description": "Read context around this line instead of an explicit range"},
                    "context_lines": {"type": "integer", "minimum": 0, "maximum": 500, "description": "Lines before and after around_line, default 5"},
                    "line_numbers": {"type": "boolean", "description": "Prefix returned lines with their 1-based numbers"},
                    "max_chars": {"type": "integer", "minimum": 100, "maximum": 500000, "description": "Maximum output characters, default 100000"},
                    "include_hash": {"type": "boolean", "description": "Include file SHA-256 and selected range metadata"}
                },
                "required": ["filepath"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="patch_file",
            description="Applies line-based edits without sending replacement content for the whole file. Supports replace, delete, insert_before and insert_after; writes atomically and returns a unified diff. All line coordinates refer to the original file.",
            inputSchema={
                "type": "object",
                "properties": {
                    "filepath": {"type": "string", "description": "Path to the UTF-8 text file"},
                    "operations": {
                        "type": "array",
                        "minItems": 1,
                        "maxItems": 100,
                        "items": {
                            "type": "object",
                            "properties": {
                                "action": {"type": "string", "enum": ["replace", "delete", "insert_before", "insert_after"]},
                                "start_line": {"type": "integer", "minimum": 1},
                                "end_line": {"type": "integer", "minimum": 1, "description": "Inclusive; used by replace/delete"},
                                "content": {"type": "string", "description": "Replacement or inserted text"},
                                "expected": {"type": "string", "description": "Optional expected original target text; aborts on mismatch"}
                            },
                            "required": ["action", "start_line"],
                            "additionalProperties": False
                        }
                    },
                    "expected_sha256": {"type": "string", "description": "Optional hash returned by read_file; aborts if the file changed"},
                    "dry_run": {"type": "boolean", "description": "Return the diff without writing"},
                    "context_lines": {"type": "integer", "minimum": 0, "maximum": 50, "description": "Unchanged context lines in the returned diff, default 3"},
                    "confirm": {"type": "boolean", "description": "Required for sensitive Windows system paths"}
                },
                "required": ["filepath", "operations"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="run_process",
            description="Runs an executable directly without starting PowerShell. Faster for known programs and scripts.",
            inputSchema={
                "type": "object",
                "properties": {
                    "program": {"type": "string", "description": "Executable or script runner, for example python, node, git"},
                    "args": {"type": "array", "items": {"type": "string"}, "description": "Argument list"},
                    "cwd": {"type": "string", "description": "Working directory"},
                    "background": {"type": "boolean", "description": "Run in background"},
                    "timeout": {"type": "number", "minimum": 1, "maximum": 600, "description": "Foreground timeout in seconds"}
                },
                "required": ["program"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="delete_file",
            description="Deletes one file after optional SHA-256 verification. Directories are refused.",
            inputSchema={
                "type": "object",
                "properties": {
                    "filepath": {"type": "string", "description": "Path to the file"},
                    "expected_sha256": {"type": "string", "description": "Abort if the current file hash differs"},
                    "confirm": {"type": "boolean", "description": "Required for sensitive system paths"}
                },
                "required": ["filepath"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="write_file",
            description="Atomically creates a UTF-8 file. Replacing an existing file requires expected_sha256 or overwrite=true.",
            inputSchema={
                "type": "object",
                "properties": {
                    "filepath": {"type": "string", "description": "Path to the file"},
                    "content": {"type": "string", "description": "Content to write"},
                    "expected_sha256": {"type": "string", "description": "Abort if an existing file hash differs"},
                    "overwrite": {"type": "boolean", "description": "Explicitly allow replacement without a hash"},
                    "confirm": {"type": "boolean", "description": "Required for sensitive system paths"}
                },
                "required": ["filepath", "content"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="list_dir",
            description="Lists the contents (files and folders) of a local directory. If empty, uses the current working directory.",
            inputSchema={
                "type": "object",
                "properties": {
                    "directory": {"type": "string", "description": "Path to list"}
                },
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="run_command",
            description="Executes a PowerShell command on Windows or a shell command on Unix. Prefer run_process for known executables.",
            inputSchema={
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Command to run"},
                    "background": {"type": "boolean", "description": "Run in background and return task_id"},
                    "cwd": {"type": "string", "description": "Working directory for the command"},
                    "timeout": {"type": "number", "minimum": 1, "maximum": 600, "description": "Foreground timeout in seconds"}
                },
                "required": ["command"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="search_files",
            description="Searches text files directly in Python without starting PowerShell. Skips dependency/cache folders and files larger than 5 MB.",
            inputSchema={
                "type": "object",
                "properties": {
                    "directory": {"type": "string", "description": "Directory to search in"},
                    "keyword": {"type": "string", "description": "Keyword to search for"},
                    "max_results": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum matches, default 50"}
                },
                "required": ["keyword"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="check_task",
            description="Check the status and output of a background task started by run_command.",
            inputSchema={
                "type": "object",
                "properties": {
                    "task_id": {"type": "string", "description": "The task ID"}
                },
                "required": ["task_id"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="cancel_task",
            description="Terminates a running background process started by run_process or run_command.",
            inputSchema={
                "type": "object",
                "properties": {
                    "task_id": {"type": "string", "description": "The task ID"}
                },
                "required": ["task_id"],
                "additionalProperties": False
            }
        ),
        types.Tool(
            name="wait_seconds",
            description="Sleep for a specified number of seconds.",
            inputSchema={
                "type": "object",
                "properties": {
                    "seconds": {"type": "number", "description": "Seconds to wait"}
                },
                "required": ["seconds"],
                "additionalProperties": False
            }
        ),
    ]

async def call_local_tool(name: str, arguments: dict):
    global CURRENT_WORKING_DIR

    if name == "set_working_directory":
        path = arguments.get("path", "")
        confirm = arguments.get("confirm", False)
        target_path = resolve_path(path)

        if not os.path.exists(target_path):
            return f"Error: Directory does not exist: {target_path}"
        if not os.path.isdir(target_path):
            return f"Error: Path is not a directory: {target_path}"

        lower_path = target_path.lower()
        if ("system32" in lower_path or "windows" in lower_path) and not confirm:
            return f"WARNING: You are trying to set the working directory to a sensitive system folder ({target_path}). If you are sure, call this tool again with confirm=true."

        CURRENT_WORKING_DIR = target_path
        return f"Working directory successfully set to: {CURRENT_WORKING_DIR}"

    elif name == "read_file":
        filepath = resolve_path(arguments.get("filepath", ""))
        start_line = arguments.get("start_line")
        end_line = arguments.get("end_line")
        around_line = arguments.get("around_line")
        context_lines = int(arguments.get("context_lines", 5))
        line_numbers = bool(arguments.get("line_numbers", False))
        max_chars = int(arguments.get("max_chars", 100000))
        include_hash = bool(arguments.get("include_hash", False))
        try:
            if not os.path.isfile(filepath):
                return f"Error: File does not exist: {filepath}"
            if around_line is not None and (start_line is not None or end_line is not None):
                return "Error: around_line cannot be combined with start_line or end_line"
            if start_line is not None and end_line is not None and end_line < start_line:
                return "Error: end_line must be greater than or equal to start_line"

            raw = await asyncio.to_thread(Path(filepath).read_bytes)
            file_hash = _sha256_bytes(raw)
            try:
                text = raw.decode("utf-8-sig")
            except UnicodeDecodeError:
                return "Error: File is not valid UTF-8 text"
            lines = text.splitlines(keepends=True)
            total_lines = len(lines)

            if around_line is not None:
                if around_line > max(1, total_lines):
                    return f"Error: around_line {around_line} is outside 1..{max(1, total_lines)}"
                selected_start = max(1, int(around_line) - context_lines)
                selected_end = min(total_lines, int(around_line) + context_lines)
            else:
                selected_start = int(start_line or 1)
                selected_end = int(end_line if end_line is not None else total_lines)

            if total_lines and selected_start > total_lines:
                return f"Error: start_line {selected_start} is outside 1..{total_lines}"
            selected_start = max(1, selected_start)
            selected_end = max(0, min(selected_end, total_lines))
            selected = lines[selected_start - 1:selected_end] if total_lines else []
            output = _format_numbered_lines(selected, selected_start) if line_numbers else "".join(selected)
            truncated = len(output) > max_chars
            if truncated:
                output = output[:max_chars] + "\n... output truncated; request a smaller line range or raise max_chars ...\n"
            if include_hash:
                metadata = (
                    f"File: {filepath}\n"
                    f"SHA-256: {file_hash}\n"
                    f"Lines: {selected_start}-{selected_end} of {total_lines}\n"
                    f"Truncated: {str(truncated).lower()}\n\n"
                )
                output = metadata + output
            return output
        except Exception as e:
            return f"Error reading file: {str(e)}"

    elif name == "patch_file":
        filepath = resolve_path(arguments.get("filepath", ""))
        operations = arguments.get("operations", [])
        expected_sha256 = arguments.get("expected_sha256")
        dry_run = bool(arguments.get("dry_run", False))
        context_lines = int(arguments.get("context_lines", 3))
        confirm = bool(arguments.get("confirm", False))
        try:
            return await asyncio.to_thread(
                _patch_file_sync,
                filepath,
                operations,
                expected_sha256,
                dry_run,
                context_lines,
                confirm,
            )
        except Exception as e:
            return f"Error patching file: {str(e)}"

    elif name == "delete_file":
        filepath = resolve_path(arguments.get("filepath", ""))
        confirm = bool(arguments.get("confirm", False))
        expected_sha256 = arguments.get("expected_sha256")
        try:
            if not os.path.exists(filepath):
                return f"Error: File does not exist: {filepath}"
            if not os.path.isfile(filepath):
                return f"Error: Refusing to delete a directory: {filepath}"
            if _is_sensitive_path(filepath) and not confirm:
                return f"WARNING: Sensitive system path. Call again with confirm=true to delete: {filepath}"
            current_bytes = Path(filepath).read_bytes()
            current_hash = _sha256_bytes(current_bytes)
            if expected_sha256 and current_hash.casefold() != str(expected_sha256).casefold():
                return (
                    "Error: File changed since it was read. "
                    f"Expected SHA-256 {expected_sha256}, current {current_hash}"
                )
            os.remove(filepath)
            return f"Successfully deleted {filepath}\nSHA-256: {current_hash}"
        except Exception as e:
            return f"Error deleting file: {str(e)}"

    elif name == "write_file":
        filepath = resolve_path(arguments.get("filepath", ""))
        content = str(arguments.get("content", ""))
        expected_sha256 = arguments.get("expected_sha256")
        overwrite = bool(arguments.get("overwrite", False))
        confirm = bool(arguments.get("confirm", False))
        try:
            if _is_sensitive_path(filepath) and not confirm:
                return f"WARNING: Sensitive system path. Call again with confirm=true to write: {filepath}"
            old_hash = None
            if os.path.exists(filepath):
                if not os.path.isfile(filepath):
                    return f"Error: Refusing to overwrite a directory: {filepath}"
                old_bytes = Path(filepath).read_bytes()
                old_hash = _sha256_bytes(old_bytes)
                if expected_sha256:
                    if old_hash.casefold() != str(expected_sha256).casefold():
                        return (
                            "Error: File changed since it was read. "
                            f"Expected SHA-256 {expected_sha256}, current {old_hash}"
                        )
                elif not overwrite:
                    return "Error: File exists; provide expected_sha256 or overwrite=true"
            elif expected_sha256:
                return "Error: expected_sha256 was provided but the file does not exist"
            written = await asyncio.to_thread(_atomic_write_utf8, filepath, content)
            new_hash = _sha256_bytes(written)
            return (
                f"Successfully wrote to {filepath}\n"
                f"Old SHA-256: {old_hash or 'none'}\n"
                f"New SHA-256: {new_hash}"
            )
        except Exception as e:
            return f"Error writing file: {str(e)}"

    elif name == "list_dir":
        directory = resolve_path(arguments.get("directory", ""))
        try:
            items = os.listdir(directory)
            return "\n".join(items) if items else "Directory is empty."
        except Exception as e:
            return f"Error listing directory: {str(e)}"

    elif name == "run_process":
        program = arguments.get("program", "")
        process_args = arguments.get("args", [])
        cwd = resolve_path(arguments.get("cwd", ""))
        bg = arguments.get("background", False)
        timeout = float(arguments.get("timeout", 120.0))
        try:
            process = await asyncio.create_subprocess_exec(
                program,
                *process_args,
                cwd=cwd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.DEVNULL,
            )

            if bg:
                task_id = str(uuid.uuid4())
                BACKGROUND_TASKS[task_id] = {"stdout": [], "stderr": [], "done": False, "exit_code": None, "process": process}

                async def monitor_process(tid, proc):
                    out, err = await proc.communicate()
                    out_text = _truncate_output(out.decode("utf-8", errors="replace") if out else "")
                    err_text = _truncate_output(err.decode("utf-8", errors="replace") if err else "")
                    BACKGROUND_TASKS[tid]["stdout"].append(out_text)
                    BACKGROUND_TASKS[tid]["stderr"].append(err_text)
                    BACKGROUND_TASKS[tid]["done"] = True
                    BACKGROUND_TASKS[tid]["exit_code"] = proc.returncode

                asyncio.create_task(monitor_process(task_id, process))
                return f"Process started in background. Task ID: {task_id}. Use check_task to monitor it."

            try:
                stdout, stderr = await asyncio.wait_for(process.communicate(), timeout=timeout)
            except asyncio.TimeoutError:
                process.kill()
                await process.wait()
                return f"Error: Process timed out after {timeout:g} seconds."

            stdout_text = stdout.decode("utf-8", errors="replace")
            stderr_text = stderr.decode("utf-8", errors="replace")
            output = stdout_text
            if stderr_text:
                output += f"\nSTDERR:\n{stderr_text}"
            if not output.strip():
                output = f"Process completed with exit code {process.returncode} (no output)."
            return _truncate_output(output)
        except Exception as e:
            return f"Error executing process: {str(e)}"

    elif name == "run_command":
        command = arguments.get("command", "")
        cwd = resolve_path(arguments.get("cwd", ""))
        bg = arguments.get("background", False)
        timeout = float(arguments.get("timeout", 120.0))
        try:
            if os.name == "nt":
                shell_argv = ["powershell", "-NoProfile", "-NonInteractive", "-Command", command]
            else:
                shell_argv = [os.environ.get("SHELL", "/bin/bash"), "-lc", command]
            process = await asyncio.create_subprocess_exec(
                *shell_argv,
                cwd=cwd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.DEVNULL
            )

            if bg:
                task_id = str(uuid.uuid4())
                BACKGROUND_TASKS[task_id] = {"stdout": [], "stderr": [], "done": False, "exit_code": None, "process": process}

                async def monitor_task(tid, proc):
                    out, err = await proc.communicate()
                    out_text = _truncate_output(out.decode("utf-8", errors="replace") if out else "")
                    err_text = _truncate_output(err.decode("utf-8", errors="replace") if err else "")
                    BACKGROUND_TASKS[tid]["stdout"].append(out_text)
                    BACKGROUND_TASKS[tid]["stderr"].append(err_text)
                    BACKGROUND_TASKS[tid]["done"] = True
                    BACKGROUND_TASKS[tid]["exit_code"] = proc.returncode

                asyncio.create_task(monitor_task(task_id, process))
                return f"Command started in background. Task ID: {task_id}. Use check_task to monitor it."

            try:
                stdout, stderr = await asyncio.wait_for(process.communicate(), timeout=timeout)
                stdout_str = stdout.decode("utf-8", errors="replace")
                stderr_str = stderr.decode("utf-8", errors="replace")

                output = stdout_str
                if stderr_str:
                    output += f"\nSTDERR:\n{stderr_str}"
                if not output.strip():
                    output = f"Command executed successfully with exit code {process.returncode} (no output)."
                return _truncate_output(output)
            except asyncio.TimeoutError:
                process.kill()
                await process.wait()
                return f"Error: Command timed out after {timeout:g} seconds."
        except Exception as e:
            return f"Error executing command: {str(e)}"

    elif name == "search_files":
        directory = resolve_path(arguments.get("directory", ""))
        keyword = arguments.get("keyword", "")
        max_results = int(arguments.get("max_results", 50))
        try:
            if not os.path.isdir(directory):
                return f"Error: Directory does not exist: {directory}"
            if not keyword:
                return "Error: keyword must not be empty"
            matches = await asyncio.to_thread(_search_files_sync, directory, keyword, max_results)
            return "\n".join(matches) if matches else "No matches found."
        except Exception as e:
            return f"Error searching files: {str(e)}"

    elif name == "check_task":
        task_id = arguments.get("task_id", "")
        if task_id not in BACKGROUND_TASKS:
            return f"Error: Task ID {task_id} not found."

        t = BACKGROUND_TASKS[task_id]
        out_str = "".join(t["stdout"]).strip()
        err_str = "".join(t["stderr"]).strip()

        status = "COMPLETED" if t["done"] else "RUNNING"
        res = f"Status: {status}\n"
        if t["done"]:
            res += f"Exit Code: {t['exit_code']}\n"
        res += f"Stdout:\n{out_str}\n"
        if err_str:
            res += f"Stderr:\n{err_str}\n"
        return _truncate_output(res)

    elif name == "cancel_task":
        task_id = arguments.get("task_id", "")
        if task_id not in BACKGROUND_TASKS:
            return f"Error: Task ID {task_id} not found."
        task = BACKGROUND_TASKS[task_id]
        process = task.get("process")
        if task.get("done") or process is None or process.returncode is not None:
            return f"Task {task_id} is already completed."
        try:
            process.terminate()
            try:
                await asyncio.wait_for(process.wait(), timeout=5.0)
            except TimeoutError:
                process.kill()
                await process.wait()
            task["done"] = True
            task["exit_code"] = process.returncode
            return f"Cancelled task {task_id}. Exit code: {process.returncode}"
        except Exception as e:
            return f"Error cancelling task: {str(e)}"

    elif name == "wait_seconds":
        sec = arguments.get("seconds", 1)
        await asyncio.sleep(sec)
        return f"Waited {sec} seconds."

    return f"Error: Unknown local tool: {name}"
