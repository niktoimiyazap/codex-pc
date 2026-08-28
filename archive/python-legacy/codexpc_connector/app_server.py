from __future__ import annotations

import asyncio
import base64
import json
import logging
import shutil
import time
from contextlib import suppress
from pathlib import Path
from typing import Any

from .logging_utils import log_event
from .security import redact


class AppServerError(RuntimeError):
    """An error returned by the Codex app-server JSON-RPC endpoint."""


class CodexAppServerClient:
    """Small JSONL client for the Codex app-server stdio protocol."""

    def __init__(self, workspace: Path, logger, request_timeout_sec: float = 120.0):
        self.workspace = workspace
        self.logger = logger
        self.request_timeout_sec = request_timeout_sec
        self.process: asyncio.subprocess.Process | None = None
        self._reader_task: asyncio.Task[None] | None = None
        self._stderr_task: asyncio.Task[None] | None = None
        self._pending: dict[int, asyncio.Future[Any]] = {}
        self._next_id = 1
        self._start_lock = asyncio.Lock()
        self._write_lock = asyncio.Lock()
        self._thread_locks: dict[str, asyncio.Lock] = {}
        self._thread_ids: dict[str, str] = {}
        self._thread_turn_locks: dict[str, asyncio.Lock] = {}
        self._turn_waiters: dict[str, asyncio.Future[dict[str, Any]]] = {}
        self._turn_text: dict[str, list[str]] = {}
        self._completed_turns: dict[str, dict[str, Any]] = {}
        self._command_streams: dict[str, dict[str, Any]] = {}
        self._stream_buffer_chars = 2 * 1024 * 1024
        self._stream_event_chars = 16 * 1024
        self._stream_emit_interval_sec = 0.08

    @property
    def running(self) -> bool:
        return self.process is not None and self.process.returncode is None

    async def start(self) -> None:
        if self.running:
            return
        async with self._start_lock:
            if self.running:
                return
            command = shutil.which("codex")
            if not command:
                raise FileNotFoundError("Codex CLI was not found in PATH")
            self.process = await asyncio.create_subprocess_exec(
                command,
                "app-server",
                "--stdio",
                cwd=str(self.workspace),
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                limit=16 * 1024 * 1024,
            )
            self._reader_task = asyncio.create_task(self._read_stdout(), name="codexpc-app-server-stdout")
            self._stderr_task = asyncio.create_task(self._read_stderr(), name="codexpc-app-server-stderr")
            try:
                await self._request_started(
                    "initialize",
                    {
                        "clientInfo": {
                            "name": "codexpc_connector",
                            "title": "CodexPC Connector",
                            "version": "0.3.0",
                        }
                    },
                )
                await self.notify("initialized", {})
            except Exception:
                await self.close()
                raise
            log_event(self.logger, logging.INFO, "codex_app_server_start")

    async def request(
        self,
        method: str,
        params: dict[str, Any] | None = None,
        *,
        timeout_sec: float | None = None,
    ) -> Any:
        await self.start()
        return await self._request_started(method, params or {}, timeout_sec=timeout_sec)

    async def _request_started(
        self,
        method: str,
        params: dict[str, Any],
        *,
        timeout_sec: float | None = None,
    ) -> Any:
        loop = asyncio.get_running_loop()
        request_id = self._next_id
        self._next_id += 1
        future = loop.create_future()
        self._pending[request_id] = future
        effective_timeout = self.request_timeout_sec if timeout_sec is None else timeout_sec
        try:
            await self._send({"method": method, "id": request_id, "params": params})
            if effective_timeout <= 0:
                return await future
            try:
                async with asyncio.timeout(effective_timeout):
                    return await future
            except TimeoutError as exc:
                with suppress(Exception):
                    await self._send({"method": "$/cancelRequest", "params": {"id": request_id}})
                if not future.done():
                    future.cancel()
                raise AppServerError(
                    f"Codex app-server request timed out after {effective_timeout:g}s: {method}"
                ) from exc
        except asyncio.CancelledError:
            with suppress(Exception):
                await self._send({"method": "$/cancelRequest", "params": {"id": request_id}})
            if not future.done():
                future.cancel()
            raise
        finally:
            self._pending.pop(request_id, None)

    async def notify(self, method: str, params: dict[str, Any] | None = None) -> None:
        await self._send({"method": method, "params": params or {}})

    async def _send(self, message: dict[str, Any]) -> None:
        process = self.process
        if not process or not process.stdin or process.returncode is not None:
            raise AppServerError("Codex app-server is not running")
        payload = (json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")
        async with self._write_lock:
            process.stdin.write(payload)
            await process.stdin.drain()

    async def _read_stdout(self) -> None:
        process = self.process
        if not process or not process.stdout:
            self._fail_pending(AppServerError("Codex app-server stdout is unavailable"))
            return
        try:
            while line := await process.stdout.readline():
                try:
                    message = json.loads(line.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                    log_event(
                        self.logger,
                        logging.ERROR,
                        "codex_app_server_invalid_json",
                        error_type=type(exc).__name__,
                    )
                    continue
                request_id = message.get("id")
                if request_id is not None and ("result" in message or "error" in message):
                    future = self._pending.get(request_id)
                    if future and not future.done():
                        if "error" in message:
                            error = message["error"]
                            future.set_exception(AppServerError(redact(error.get("message", str(error)))))
                        else:
                            future.set_result(message.get("result"))
                elif request_id is not None and "method" in message:
                    method = str(message["method"])
                    if method == "roots/list":
                        await self._send(
                            {
                                "id": request_id,
                                "result": {
                                    "roots": [
                                        {
                                            "uri": self.workspace.as_uri(),
                                            "name": self.workspace.name or "CodexPC Workspace",
                                        }
                                    ]
                                },
                            }
                        )
                    else:
                        await self._reply_unsupported(request_id, method)
                elif request_id is None and "method" in message:
                    self._handle_notification(str(message["method"]), message.get("params") or {})
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            log_event(
                self.logger,
                logging.ERROR,
                "codex_app_server_reader_error",
                error_type=type(exc).__name__,
                error=redact(str(exc))[:1000],
            )
        finally:
            self._fail_pending(AppServerError("Codex app-server connection closed"))


    def begin_command_stream(self, process_id: str, call_id: str) -> None:
        self._command_streams[process_id] = {
            "call_id": call_id,
            "stdout": [],
            "stderr": [],
            "stdout_chars": 0,
            "stderr_chars": 0,
            "pending_stdout": "",
            "pending_stderr": "",
            "last_emit": 0.0,
            "truncated": False,
        }

    def _append_stream_buffer(self, state: dict[str, Any], stream: str, delta: str) -> str:
        count_key = f"{stream}_chars"
        current = int(state.get(count_key, 0))
        remaining = self._stream_buffer_chars - current
        if remaining <= 0:
            state["truncated"] = True
            return ""
        piece = delta[:remaining]
        if piece:
            state[stream].append(piece)
            state[count_key] = current + len(piece)
        if len(piece) < len(delta):
            state["truncated"] = True
        return piece

    def _emit_stream_pending(self, state: dict[str, Any], process_id: str, stream: str, *, force: bool = False) -> None:
        pending_key = f"pending_{stream}"
        pending = str(state.get(pending_key) or "")
        if not pending:
            return
        now = time.monotonic()
        if not force and now - float(state.get("last_emit", 0.0)) < self._stream_emit_interval_sec:
            return
        emitted = pending[: self._stream_event_chars]
        state[pending_key] = pending[len(emitted) :]
        state["last_emit"] = now
        log_event(
            self.logger,
            logging.INFO,
            "chatgpt_tool_call_stream",
            tool="command_exec",
            call_id=state["call_id"],
            process_id=process_id,
            stream=stream,
            delta=emitted,
            cap_reached=bool(state.get("truncated", False)),
        )

    def end_command_stream(self, process_id: str) -> dict[str, str]:
        state = self._command_streams.pop(process_id, None) or {}
        for stream in ("stdout", "stderr"):
            while state.get(f"pending_{stream}"):
                self._emit_stream_pending(state, process_id, stream, force=True)
        marker = "\n... connector stream buffer truncated ...\n" if state.get("truncated") else ""
        stdout = "".join(state.get("stdout", []))
        stderr = "".join(state.get("stderr", []))
        if marker:
            if stderr and not stdout:
                stderr += marker
            else:
                stdout += marker
        return {"stdout": stdout, "stderr": stderr}

    def _handle_notification(self, method: str, params: dict[str, Any]) -> None:
        if method == "command/exec/outputDelta":
            process_id = str(params.get("processId") or "")
            state = self._command_streams.get(process_id)
            stream = str(params.get("stream") or "stdout")
            encoded = params.get("deltaBase64")
            if state is not None and stream in {"stdout", "stderr"} and isinstance(encoded, str):
                try:
                    delta = base64.b64decode(encoded).decode("utf-8", errors="replace")
                except (ValueError, TypeError):
                    delta = ""
                if delta:
                    accepted = self._append_stream_buffer(state, stream, delta)
                    pending_key = f"pending_{stream}"
                    if accepted:
                        state[pending_key] = str(state.get(pending_key) or "") + accepted
                    if params.get("capReached"):
                        state["truncated"] = True
                    self._emit_stream_pending(state, process_id, stream)
            return
        turn_id = str(params.get("turnId") or "")
        if method == "item/agentMessage/delta" and turn_id:
            delta = params.get("delta")
            if isinstance(delta, str):
                self._turn_text.setdefault(turn_id, []).append(delta)
            return
        if method == "item/completed" and turn_id:
            item = params.get("item")
            if isinstance(item, dict) and item.get("type") == "agentMessage":
                text = item.get("text")
                if isinstance(text, str) and not self._turn_text.get(turn_id):
                    self._turn_text.setdefault(turn_id, []).append(text)
            return
        if method == "turn/completed":
            turn = params.get("turn")
            if not turn_id and isinstance(turn, dict):
                turn_id = str(turn.get("id") or "")
            future = self._turn_waiters.pop(turn_id, None)
            text = "".join(self._turn_text.pop(turn_id, []))
            completed = {"turn": turn, "text": text}
            if future and not future.done():
                future.set_result(completed)
            elif turn_id:
                self._completed_turns[turn_id] = completed

    async def run_turn(
        self,
        thread_id: str,
        instruction: str,
        *,
        output_schema: dict[str, Any] | None = None,
        timeout_sec: float | None = None,
    ) -> dict[str, Any]:
        lock = self._thread_turn_locks.setdefault(thread_id, asyncio.Lock())
        async with lock:
            payload: dict[str, Any] = {
                "threadId": thread_id,
                "input": [{"type": "text", "text": instruction, "text_elements": []}],
            }
            if output_schema is not None:
                payload["outputSchema"] = output_schema
            result = await self.request("turn/start", payload, timeout_sec=timeout_sec)
            try:
                turn_id = str(result["turn"]["id"])
            except (KeyError, TypeError) as exc:
                raise AppServerError("Codex app-server returned no turn id") from exc
            loop = asyncio.get_running_loop()
            future: asyncio.Future[dict[str, Any]] = loop.create_future()
            self._turn_waiters[turn_id] = future
            self._turn_text.setdefault(turn_id, [])
            completed = self._completed_turns.pop(turn_id, None)
            if completed is not None and not future.done():
                self._turn_waiters.pop(turn_id, None)
                future.set_result(completed)
            effective_timeout = self.request_timeout_sec if timeout_sec is None else timeout_sec
            try:
                if effective_timeout <= 0:
                    return await future
                async with asyncio.timeout(effective_timeout):
                    completed = await future
                    if not completed.get("text"):
                        with suppress(Exception):
                            history = await self.request(
                                "thread/read",
                                {"threadId": thread_id, "includeTurns": True},
                                timeout_sec=min(15.0, effective_timeout),
                            )
                            completed["text"] = self._latest_agent_text(history, turn_id)
                    return completed
            except TimeoutError as exc:
                self._turn_waiters.pop(turn_id, None)
                self._turn_text.pop(turn_id, None)
                with suppress(Exception):
                    await self.request("turn/interrupt", {"threadId": thread_id, "turnId": turn_id}, timeout_sec=5)
                raise AppServerError(f"Codex browser turn timed out after {effective_timeout:g}s") from exc


    @staticmethod
    def _latest_agent_text(history: Any, turn_id: str) -> str:
        if not isinstance(history, dict):
            return ""
        thread = history.get("thread")
        if not isinstance(thread, dict):
            return ""
        turns = thread.get("turns")
        if not isinstance(turns, list):
            return ""
        candidates = [turn for turn in turns if isinstance(turn, dict)]
        matching = [turn for turn in candidates if str(turn.get("id") or "") == turn_id]
        selected = matching[-1:] or candidates[-1:]
        for turn in reversed(selected):
            items = turn.get("items")
            if not isinstance(items, list):
                continue
            for item in reversed(items):
                if isinstance(item, dict) and item.get("type") == "agentMessage":
                    text = item.get("text")
                    if isinstance(text, str):
                        return text
        return ""

    async def _reply_unsupported(self, request_id: int | str, method: str) -> None:
        log_event(self.logger, logging.WARNING, "codex_app_server_request_rejected", method=method)
        with suppress(Exception):
            await self._send(
                {
                    "id": request_id,
                    "error": {"code": -32601, "message": f"Unsupported server request: {method}"},
                }
            )

    async def _read_stderr(self) -> None:
        process = self.process
        if not process or not process.stderr:
            return
        try:
            while line := await process.stderr.readline():
                message = redact(line.decode("utf-8", errors="replace").strip())
                if message:
                    log_event(self.logger, logging.WARNING, "codex_app_server_stderr", stderr_preview=message[:1000])
        except asyncio.CancelledError:
            raise

    def _fail_pending(self, exc: Exception) -> None:
        for future in list(self._pending.values()):
            if not future.done():
                future.set_exception(exc)

    async def ensure_thread(self, session_key: str = "default") -> str:
        key = session_key.strip() or "default"
        existing = self._thread_ids.get(key)
        if existing:
            return existing
        lock = self._thread_locks.setdefault(key, asyncio.Lock())
        async with lock:
            existing = self._thread_ids.get(key)
            if existing:
                return existing
            result = await self.request(
                "thread/start",
                {
                    "cwd": str(self.workspace),
                    "ephemeral": True,
                    "sandbox": "danger-full-access",
                    "approvalPolicy": "never",
                },
            )
            try:
                thread_id = str(result["thread"]["id"])
            except (KeyError, TypeError) as exc:
                raise AppServerError("Codex app-server returned no thread id") from exc
            self._thread_ids[key] = thread_id
            return thread_id

    async def close(self) -> None:
        process = self.process
        self.process = None
        self._thread_ids.clear()
        self._thread_locks.clear()
        self._thread_turn_locks.clear()
        for future in self._turn_waiters.values():
            if not future.done():
                future.set_exception(AppServerError("Codex app-server stopped"))
        self._turn_waiters.clear()
        self._turn_text.clear()
        self._completed_turns.clear()
        self._command_streams.clear()
        if process and process.returncode is None:
            if process.stdin:
                process.stdin.close()
                with suppress(BrokenPipeError, ConnectionResetError):
                    await process.stdin.wait_closed()
            process.terminate()
            with suppress(TimeoutError):
                await asyncio.wait_for(process.wait(), timeout=3)
            if process.returncode is None:
                process.kill()
                await process.wait()
        current = asyncio.current_task()
        for task in (self._reader_task, self._stderr_task):
            if task and task is not current and not task.done():
                task.cancel()
                with suppress(asyncio.CancelledError):
                    await task
        self._reader_task = None
        self._stderr_task = None
        self._fail_pending(AppServerError("Codex app-server stopped"))
