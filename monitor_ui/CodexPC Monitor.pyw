from __future__ import annotations

import base64
import ctypes
import json
import mimetypes
import os
import re
import secrets
import hmac
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request
import webbrowser
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HOST = "127.0.0.1"
PORT = 8765
STATE_DIR = Path(os.environ.get("LOCALAPPDATA", Path.home())) / "CodexPCConnector"
RESTART_REQUEST_PATH = STATE_DIR / "restart.request"
LOG_PATH = STATE_DIR / "logs" / "connector.jsonl"
HISTORY_OFFSET_PATH = STATE_DIR / "frontend-history.offset"
SESSIONS_PATH = STATE_DIR / "sessions.json"
DELETED_SESSIONS_DIR = STATE_DIR / "deleted-sessions"
SESSION_STATE_LOCK = threading.Lock()
APPROVAL_DIR = STATE_DIR / "approvals"
SECRET_DIR = STATE_DIR / "secrets"
SECRET_VAULT_PATH = SECRET_DIR / "vault.json"
SECRET_HISTORY_PATH = SECRET_DIR / "history.jsonl"
SECRET_REQUEST_DIR = SECRET_DIR / "requests"
SECRET_RESPONSE_DIR = SECRET_DIR / "responses"
FRONTEND_AUTH_PATH = STATE_DIR / "frontend-auth.dpapi"
START_TUNNEL_SCRIPT = Path.home() / "Desktop" / "Start Codex Tunnel.cmd"

UI_DIR = Path(__file__).resolve().parent
PAGE_PATH = UI_DIR / "monitor.html"
SCRIPT_PATH = UI_DIR / "monitor.js"


class DATA_BLOB(ctypes.Structure):
    _fields_ = [("cbData", ctypes.c_ulong), ("pbData", ctypes.POINTER(ctypes.c_ubyte))]


def _blob(data: bytes) -> tuple[DATA_BLOB, ctypes.Array]:
    buf = ctypes.create_string_buffer(data)
    return DATA_BLOB(len(data), ctypes.cast(buf, ctypes.POINTER(ctypes.c_ubyte))), buf


def dpapi_protect(value: str) -> str:
    raw = value.encode("utf-8")
    in_blob, keepalive = _blob(raw)
    out_blob = DATA_BLOB()
    if not ctypes.windll.crypt32.CryptProtectData(ctypes.byref(in_blob), "CodexPC Secret Vault", None, None, None, 0, ctypes.byref(out_blob)):
        raise OSError("CryptProtectData failed")
    try:
        encrypted = ctypes.string_at(out_blob.pbData, out_blob.cbData)
        return base64.b64encode(encrypted).decode("ascii")
    finally:
        ctypes.windll.kernel32.LocalFree(out_blob.pbData)
        _ = keepalive


def dpapi_unprotect(ciphertext: str) -> str:
    raw = base64.b64decode(ciphertext.encode("ascii"), validate=True)
    in_blob, keepalive = _blob(raw)
    out_blob = DATA_BLOB()
    if not ctypes.windll.crypt32.CryptUnprotectData(ctypes.byref(in_blob), None, None, None, None, 0, ctypes.byref(out_blob)):
        raise OSError("CryptUnprotectData failed")
    try:
        return ctypes.string_at(out_blob.pbData, out_blob.cbData).decode("utf-8")
    finally:
        ctypes.windll.kernel32.LocalFree(out_blob.pbData)
        _ = keepalive


def load_frontend_auth_token() -> str:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    try:
        encrypted = FRONTEND_AUTH_PATH.read_text(encoding="ascii").strip()
        token = dpapi_unprotect(encrypted)
        if len(token) >= 32:
            return token
    except (OSError, ValueError, UnicodeError):
        pass
    token = secrets.token_urlsafe(32)
    tmp = STATE_DIR / f".frontend-auth.{os.getpid()}.tmp"
    tmp.write_text(dpapi_protect(token), encoding="ascii")
    os.replace(tmp, FRONTEND_AUTH_PATH)
    return token


FRONTEND_AUTH_TOKEN = load_frontend_auth_token()


def load_secret_vault() -> dict:
    try:
        payload = json.loads(SECRET_VAULT_PATH.read_text(encoding="utf-8"))
        if isinstance(payload, dict) and isinstance(payload.get("secrets"), list):
            return payload
    except (OSError, json.JSONDecodeError):
        pass
    return {"version": 1, "secrets": []}


def save_secret_vault(payload: dict) -> None:
    SECRET_DIR.mkdir(parents=True, exist_ok=True)
    tmp = SECRET_DIR / f".vault.{os.getpid()}.tmp"
    tmp.write_text(json.dumps(payload, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    os.replace(tmp, SECRET_VAULT_PATH)


def infer_secret_metadata(value: str) -> tuple[str, str]:
    text = value.strip()
    lower = text.lower()
    if re.fullmatch(r"\d{6,12}:[A-Za-z0-9_-]{30,}", text):
        return "Telegram bot token", "telegram bot credential"
    if text.startswith(("sk-proj-", "sk-")):
        return "OpenAI API key", "openai api credential"
    if text.startswith(("ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_")):
        return "GitHub token", "github credential"
    if text.startswith(("xoxb-", "xoxp-", "xoxa-", "xoxr-", "xoxs-")):
        return "Slack token", "slack credential"
    if text.startswith("AKIA") and len(text) == 20:
        return "AWS access key", "aws credential"
    if text.startswith("AIza") and len(text) >= 30:
        return "Google API key", "google api credential"
    if text.count(".") == 2 and all(re.fullmatch(r"[A-Za-z0-9_-]+", part or "") for part in text.split(".")):
        return "JWT / access token", "jwt-style credential"
    if re.match(r"^[a-z][a-z0-9+.-]*://", lower):
        return "Connection URL", "service connection credential"
    if "@" in text and re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", text):
        return "Account identifier", "email-like account value"
    if len(text) <= 40 and not re.search(r"\s", text):
        return "Password / short token", "short opaque credential"
    return "Opaque secret", "generic saved credential"


def ensure_secret_id(item: dict) -> str:
    sid = str(item.get("id", "")).strip()
    if sid:
        return sid
    legacy = str(item.get("name", "")).strip()
    if legacy:
        sid = legacy
    else:
        sid = f"vault-{time.time_ns()}"
    item["id"] = sid
    return sid


def secret_public_record(item: dict) -> dict:
    sid = ensure_secret_id(item)
    return {key: item.get(key) for key in ("id", "title", "kind", "hint", "last_purpose", "created_at", "updated_at", "last_used_at", "use_count") if item.get(key) not in (None, "")} | {"id": sid}


def append_secret_history(action: str, name: str, request_id: str = "", purpose: str = "") -> None:
    SECRET_DIR.mkdir(parents=True, exist_ok=True)
    event = {"time": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "action": action, "name": name}
    if request_id:
        event["request_id"] = request_id
    if purpose:
        event["purpose"] = purpose[:240]
    with SECRET_HISTORY_PATH.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(event, ensure_ascii=False) + "\n")


def read_secret_history(limit: int = 80) -> list[dict]:
    if not SECRET_HISTORY_PATH.exists():
        return []
    lines: deque[str] = deque(maxlen=limit)
    try:
        with SECRET_HISTORY_PATH.open("r", encoding="utf-8", errors="replace") as handle:
            lines.extend(handle)
    except OSError:
        return []
    result = []
    for line in lines:
        try:
            result.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return list(reversed(result))


def pending_secret_requests() -> list[dict]:
    if not SECRET_REQUEST_DIR.exists():
        return []
    out = []
    for path in sorted(SECRET_REQUEST_DIR.glob("secret-*.json"), key=lambda p: p.stat().st_mtime, reverse=True):
        try:
            item = json.loads(path.read_text(encoding="utf-8"))
            if not (SECRET_RESPONSE_DIR / path.name).exists():
                out.append(item)
        except (OSError, json.JSONDecodeError):
            continue
    return out[:20]


def read_page() -> str:
    return PAGE_PATH.read_text(encoding="utf-8")


def session_is_deleted(session_id: str) -> bool:
    return bool(re.fullmatch(r"session-[0-9]+", session_id)) and (DELETED_SESSIONS_DIR / session_id).exists()


def delete_persistent_session(session_id: str) -> None:
    if not re.fullmatch(r"session-[0-9]+", session_id):
        raise ValueError("invalid session id")
    with SESSION_STATE_LOCK:
        DELETED_SESSIONS_DIR.mkdir(parents=True, exist_ok=True)
        marker = DELETED_SESSIONS_DIR / session_id
        marker_tmp = DELETED_SESSIONS_DIR / f".{session_id}.{os.getpid()}.tmp"
        marker_tmp.write_text("deleted\n", encoding="utf-8")
        os.replace(marker_tmp, marker)

        try:
            payload = json.loads(SESSIONS_PATH.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            payload = {"version": 1, "sessions": []}
        raw_sessions = payload.get("sessions", []) if isinstance(payload, dict) else []
        if not isinstance(raw_sessions, list):
            raw_sessions = []
        payload = {
            "version": 1,
            "sessions": [
                item for item in raw_sessions
                if not isinstance(item, dict) or str(item.get("id", "")).strip() != session_id
            ],
        }
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        tmp = STATE_DIR / f".sessions.{os.getpid()}.{time.time_ns()}.tmp"
        tmp.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        os.replace(tmp, SESSIONS_PATH)


def read_sessions() -> list[dict]:
    """Read persistent connector sessions, then enrich them from the event log.

    sessions.json is the source of truth. The log is only a compatibility/freshness
    layer so a frontend restart or connector restart never deletes the user's chat
    list just because a new connector_start event appeared.
    """
    sessions: dict[str, dict] = {}
    try:
        payload = json.loads(SESSIONS_PATH.read_text(encoding="utf-8"))
        if isinstance(payload, dict) and payload.get("version") == 1 and isinstance(payload.get("sessions"), list):
            for raw in payload["sessions"]:
                if not isinstance(raw, dict):
                    continue
                sid = str(raw.get("id", "")).strip()
                name = str(raw.get("name", "")).strip()
                if not sid or not name or session_is_deleted(sid):
                    continue
                created_at = str(raw.get("created_at", ""))
                sessions[sid] = {
                    "id": sid,
                    "name": name,
                    "created_at": created_at,
                    "updated_at": str(raw.get("updated_at", "") or created_at),
                }
    except (OSError, json.JSONDecodeError):
        pass

    if LOG_PATH.exists():
        try:
            with LOG_PATH.open("r", encoding="utf-8", errors="replace") as handle:
                for line in handle:
                    try:
                        event = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if not isinstance(event, dict):
                        continue
                    message = str(event.get("message", ""))
                    data = event.get("data", {})
                    if not isinstance(data, dict):
                        continue
                    sid = str(data.get("session_id", "")).strip()
                    if not sid or session_is_deleted(sid):
                        continue
                    event_time = str(event.get("time", ""))
                    if message == "chat_session_created" and sid not in sessions:
                        name = str(data.get("session_name", "")).strip()
                        if name:
                            created_at = str(data.get("created_at", "") or event_time)
                            sessions[sid] = {
                                "id": sid,
                                "name": name,
                                "created_at": created_at,
                                "updated_at": created_at,
                            }
                    item = sessions.get(sid)
                    if item and event_time and event_time > str(item.get("updated_at", "")):
                        item["updated_at"] = event_time
        except OSError:
            pass

    result = list(sessions.values())
    result.sort(key=lambda item: str(item.get("updated_at", "")), reverse=True)
    return result


def load_history_offsets() -> dict[str, int]:
    if not HISTORY_OFFSET_PATH.exists():
        return {}
    try:
        raw = HISTORY_OFFSET_PATH.read_text(encoding="utf-8").strip()
        if not raw:
            return {}
        # Backward compatibility with the old single global byte offset.
        if raw.isdigit():
            return {"*": max(0, int(raw))}
        parsed = json.loads(raw)
        if not isinstance(parsed, dict):
            return {}
        return {str(key): max(0, int(value)) for key, value in parsed.items() if str(value).isdigit()}
    except (OSError, ValueError, TypeError, json.JSONDecodeError):
        return {}


def save_history_offsets(offsets: dict[str, int]) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    tmp = STATE_DIR / f".frontend-history.{os.getpid()}.tmp"
    tmp.write_text(json.dumps(offsets, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    os.replace(tmp, HISTORY_OFFSET_PATH)


def read_history(limit: int = 120, session_id: str = "") -> list[dict]:
    if not LOG_PATH.exists():
        return []
    offsets = load_history_offsets()
    offset = max(offsets.get("*", 0), offsets.get(session_id, 0) if session_id else 0)
    result: deque[dict] = deque(maxlen=limit)
    try:
        size = LOG_PATH.stat().st_size
        if offset > size:
            offset = 0
        with LOG_PATH.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(offset)
            for line in handle:
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if session_id:
                    data = event.get("data", {}) if isinstance(event, dict) else {}
                    if not isinstance(data, dict) or str(data.get("session_id", "")) != session_id:
                        continue
                result.append(event)
    except OSError:
        return []
    return list(result)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_: object) -> None:
        return

    def is_authorized(self) -> bool:
        cookie = self.headers.get("Cookie", "")
        match = re.search(r"(?:^|;\s*)codexpc_auth=([^;]+)", cookie)
        return bool(match and hmac.compare_digest(match.group(1), FRONTEND_AUTH_TOKEN))

    def is_same_origin_frontend(self) -> bool:
        origin = self.headers.get("Origin", "").strip()
        if not origin:
            return False
        try:
            parsed = urllib.parse.urlsplit(origin)
            return parsed.scheme == "http" and parsed.hostname == HOST and parsed.port == PORT
        except (TypeError, ValueError):
            return False

    def end_headers(self) -> None:
        if getattr(self, "_refresh_auth_cookie", False):
            self.send_header("Set-Cookie", f"codexpc_auth={FRONTEND_AUTH_TOKEN}; Path=/; HttpOnly; SameSite=Strict")
            self._refresh_auth_cookie = False
        super().end_headers()

    def reject_unauthorized(self) -> None:
        body = b"CodexPC frontend authorization required"
        self.send_response(401)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def send_json(self, payload: object, status: int = 200) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def read_json(self, max_bytes: int = 65536) -> dict:
        length = min(int(self.headers.get("Content-Length", "0") or "0"), max_bytes)
        payload = json.loads(self.rfile.read(length).decode("utf-8"))
        if not isinstance(payload, dict):
            raise ValueError("JSON object required")
        return payload

    def do_POST(self) -> None:
        if not self.is_authorized():
            # A frontend tab can outlive a monitor restart. Its HttpOnly cookie then
            # contains the previous per-instance token and every Approve/Deny POST
            # used to fail with 401. A same-origin request is already constrained to
            # the loopback UI, so accept it once and rotate the cookie to this
            # monitor instance. Cross-origin and raw unauthenticated requests remain
            # rejected.
            if not self.is_same_origin_frontend():
                self.reject_unauthorized()
                return
            self._refresh_auth_cookie = True
        if self.path == "/secrets/save":
            try:
                payload = self.read_json()
                title = str(payload.get("title", "")).strip()[:120]
                value = str(payload.get("value", ""))
                if not value:
                    raise ValueError("secret value is required")
                vault = load_secret_vault()
                now = time.strftime("%Y-%m-%dT%H:%M:%S%z")
                kind, hint = infer_secret_metadata(value)
                sid = f"vault-{time.time_ns()}"
                existing = {"id": sid, "title": title, "kind": kind, "hint": hint, "created_at": now, "use_count": 0, "ciphertext": dpapi_protect(value), "updated_at": now}
                vault["secrets"].append(existing)
                save_secret_vault(vault)
                append_secret_history("saved", sid)
                self.send_json({"ok": True, "secret": secret_public_record(existing)})
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            return
        if self.path == "/secrets/reveal":
            try:
                payload = self.read_json()
                sid = str(payload.get("id", "")).strip()
                vault = load_secret_vault()
                record = next((x for x in vault["secrets"] if ensure_secret_id(x).casefold() == sid.casefold()), None)
                if record is None:
                    raise ValueError("secret not found")
                value = dpapi_unprotect(str(record.get("ciphertext", "")))
                append_secret_history("revealed", sid)
                self.send_json({"ok": True, "id": sid, "value": value})
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            return
        if self.path == "/secrets/delete":
            try:
                payload = self.read_json()
                sid = str(payload.get("id", payload.get("name", ""))).strip()
                vault = load_secret_vault()
                before = len(vault["secrets"])
                vault["secrets"] = [x for x in vault["secrets"] if ensure_secret_id(x).casefold() != sid.casefold()]
                if len(vault["secrets"]) == before:
                    raise ValueError("secret not found")
                save_secret_vault(vault)
                append_secret_history("deleted", sid)
                self.send_json({"ok": True})
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            return
        if self.path == "/secret-request":
            try:
                payload = self.read_json()
                request_id = str(payload.get("request_id", "")).strip()
                approve = bool(payload.get("approve", False))
                if not re.fullmatch(r"secret-[0-9]{6,}", request_id):
                    raise ValueError("invalid request id")
                request_path = SECRET_REQUEST_DIR / f"{request_id}.json"
                request = json.loads(request_path.read_text(encoding="utf-8"))
                sid = str(request.get("id", request.get("name", "")))
                purpose = str(request.get("purpose", ""))
                response = {"approved": approve, "name": sid, "id": sid}
                if approve:
                    vault = load_secret_vault()
                    record = next((x for x in vault["secrets"] if ensure_secret_id(x).casefold() == sid.casefold()), None)
                    if record is None:
                        raise ValueError("saved secret no longer exists")
                    response["secret"] = dpapi_unprotect(str(record.get("ciphertext", "")))
                    now = time.strftime("%Y-%m-%dT%H:%M:%S%z")
                    record["last_used_at"] = now
                    record["last_purpose"] = purpose[:240]
                    record["use_count"] = int(record.get("use_count", 0) or 0) + 1
                    save_secret_vault(vault)
                    append_secret_history("inserted", sid, request_id, purpose)
                else:
                    response["reason"] = "Denied in CodexPC frontend"
                    append_secret_history("denied", sid, request_id, purpose)
                SECRET_RESPONSE_DIR.mkdir(parents=True, exist_ok=True)
                target = SECRET_RESPONSE_DIR / f"{request_id}.json"
                tmp = SECRET_RESPONSE_DIR / f".{request_id}.{os.getpid()}.tmp"
                tmp.write_text(json.dumps(response, ensure_ascii=False), encoding="utf-8")
                os.replace(tmp, target)
                self.send_json({"ok": True, "approved": approve, "request_id": request_id})
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            return
        if self.path == "/approval":
            try:
                length = min(int(self.headers.get("Content-Length", "0") or "0"), 8192)
                payload = json.loads(self.rfile.read(length).decode("utf-8"))
                approval_id = str(payload.get("approval_id", ""))
                approve = bool(payload.get("approve", False))
                reason = str(payload.get("reason", ""))[:300]
                if not approval_id.startswith("approval-") or not approval_id[9:].isdigit():
                    raise ValueError("invalid approval id")
                APPROVAL_DIR.mkdir(parents=True, exist_ok=True)
                target = APPROVAL_DIR / f"{approval_id}.json"
                tmp = APPROVAL_DIR / f".{approval_id}.{os.getpid()}.tmp"
                tmp.write_text(json.dumps({"approve": approve, "reason": reason}, ensure_ascii=False), encoding="utf-8")
                os.replace(tmp, target)
                body = json.dumps({"ok": True, "approval_id": approval_id, "approve": approve}).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            except Exception as exc:
                body = json.dumps({"ok": False, "error": str(exc)}).encode("utf-8")
                self.send_response(400)
                self.send_header("Content-Type", "application/json; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            return
        if self.path == "/sessions/delete":
            try:
                payload = self.read_json()
                session_id = str(payload.get("session_id", "")).strip()
                delete_persistent_session(session_id)
                self.send_json({"ok": True, "session_id": session_id})
            except ValueError as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 500)
            return
        if self.path == "/history/clear":
            try:
                payload = self.read_json()
                session_id = str(payload.get("session_id", "")).strip()
                if not session_id:
                    raise ValueError("session_id is required")
                offset = LOG_PATH.stat().st_size if LOG_PATH.exists() else 0
                offsets = load_history_offsets()
                offsets[session_id] = offset
                save_history_offsets(offsets)
                self.send_json({"ok": True, "session_id": session_id, "offset": offset})
            except ValueError as exc:
                self.send_json({"ok": False, "error": str(exc)}, 400)
            except Exception as exc:
                self.send_json({"ok": False, "error": str(exc)}, 500)
            return
        if self.path == "/restart":
            body = b"restarting connector"
            self.send_response(202)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            self.wfile.flush()

            repo = Path(__file__).resolve().parent.parent
            restart_script = repo / "scripts" / "restart-connector.ps1"
            subprocess.Popen(
                ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", str(restart_script)],
                creationflags=getattr(subprocess, "DETACHED_PROCESS", 0) | getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0),
                close_fds=True,
            )
            return
        if self.path == "/restart-frontend":
            body = b"restarting frontend"
            self.send_response(202)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            self.wfile.flush()

            threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        self.send_error(404)

    def do_GET(self) -> None:
        parsed_url = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed_url.query)
        bootstrap = query.get("auth", [""])[0]
        if parsed_url.path == "/health" and bootstrap and hmac.compare_digest(bootstrap, FRONTEND_AUTH_TOKEN):
            self.send_response(204)
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            return
        if parsed_url.path == "/" and bootstrap and hmac.compare_digest(bootstrap, FRONTEND_AUTH_TOKEN):
            self.send_response(302)
            self.send_header("Location", "/")
            self.send_header("Set-Cookie", f"codexpc_auth={FRONTEND_AUTH_TOKEN}; Path=/; HttpOnly; SameSite=Strict")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            return
        if not self.is_authorized():
            self.reject_unauthorized()
            return
        if self.path == "/":
            body = read_page().encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/monitor.js":
            try:
                body = SCRIPT_PATH.read_bytes()
            except OSError:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/javascript; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/instance":
            self.send_json({"pid": os.getpid()})
            return
        if self.path == "/connector-instance":
            pids = []
            try:
                result = subprocess.run(
                    ["powershell.exe", "-NoProfile", "-Command", "Get-Process codexpc-go -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id"],
                    capture_output=True,
                    text=True,
                    timeout=1.5,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                )
                for raw in result.stdout.splitlines():
                    raw = raw.strip()
                    if raw.isdigit():
                        pids.append(int(raw))
            except Exception:
                pids = []
            self.send_json({"pids": pids, "pid": pids[0] if pids else 0, "alive": bool(pids)})
            return
        if parsed_url.path == "/sessions":
            self.send_json({"sessions": read_sessions()})
            return
        if parsed_url.path == "/history":
            session_id = query.get("session_id", [""])[0]
            body = json.dumps(read_history(session_id=session_id), ensure_ascii=False).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path == "/secrets":
            vault = load_secret_vault()
            self.send_json({"secrets": [secret_public_record(x) for x in vault.get("secrets", [])]})
            return
        if self.path == "/secrets/history":
            self.send_json({"history": read_secret_history()})
            return
        if self.path == "/secret-requests":
            self.send_json({"requests": pending_secret_requests()})
            return
        if self.path.startswith("/process-alive?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            try:
                pid = int(query.get("pid", ["0"])[0])
            except ValueError:
                pid = 0
            alive = False
            if pid > 0 and os.name == "nt":
                try:
                    import ctypes
                    PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
                    handle = ctypes.windll.kernel32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, pid)
                    if handle:
                        alive = True
                        ctypes.windll.kernel32.CloseHandle(handle)
                except Exception:
                    alive = False
            body = json.dumps({"pid": pid, "alive": alive}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.startswith("/text?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            requested = query.get("path", [""])[0]
            try:
                target = Path(requested).expanduser().resolve(strict=True)
                target.relative_to(Path.home().resolve())
            except (OSError, ValueError):
                self.send_error(404)
                return
            if not target.is_file() or target.stat().st_size > 2 * 1024 * 1024:
                self.send_error(404)
                return
            try:
                text = target.read_text(encoding="utf-8-sig", errors="replace")
            except OSError:
                self.send_error(404)
                return
            normalized = text.replace("\r\n", "\n")
            lines = normalized.split("\n")
            if normalized.endswith("\n") and lines and lines[-1] == "":
                lines.pop()
            total_lines = len(lines)
            try:
                offset = max(1, int(query.get("offset", ["1"])[0]))
                raw_limit = query.get("limit", [""])[0]
                limit = max(1, min(2000, int(raw_limit))) if raw_limit else None
            except ValueError:
                self.send_error(400)
                return
            start = min(total_lines, offset - 1)
            end = min(total_lines, start + limit) if limit else total_lines
            body = "\n".join(lines[start:end]).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Total-Lines", str(total_lines))
            self.send_header("X-Start-Line", str(start + 1))
            self.send_header("X-End-Line", str(end))
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.startswith("/media?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            requested = query.get("path", [""])[0]
            try:
                target = Path(requested).expanduser().resolve(strict=True)
                target.relative_to(Path.home().resolve())
            except (OSError, ValueError):
                self.send_error(404)
                return
            allowed = {
                ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".svg",
                ".mp4", ".webm", ".mov", ".m4v", ".avi", ".mkv",
                ".mp3", ".wav", ".ogg", ".m4a", ".aac", ".flac",
            }
            if not target.is_file() or target.suffix.casefold() not in allowed:
                self.send_error(404)
                return
            content_type = mimetypes.guess_type(target.name)[0] or "application/octet-stream"
            size = target.stat().st_size
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(size))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            with target.open("rb") as handle:
                while chunk := handle.read(256 * 1024):
                    self.wfile.write(chunk)
            return
        if self.path == "/events":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            position = LOG_PATH.stat().st_size if LOG_PATH.exists() else 0
            try:
                while True:
                    if LOG_PATH.exists():
                        size = LOG_PATH.stat().st_size
                        if size < position:
                            position = 0
                        if size > position:
                            with LOG_PATH.open("r", encoding="utf-8", errors="replace") as handle:
                                handle.seek(position)
                                for line in handle:
                                    try:
                                        event = json.loads(line)
                                    except json.JSONDecodeError:
                                        continue
                                    payload = json.dumps(event, ensure_ascii=False)
                                    self.wfile.write(f"data: {payload}\n\n".encode("utf-8"))
                                position = handle.tell()
                            self.wfile.flush()
                    self.wfile.write(b": keepalive\n\n")
                    self.wfile.flush()
                    time.sleep(0.08)
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass
            return
        self.send_error(404)


def already_running() -> bool:
    try:
        url = f"http://{HOST}:{PORT}/health?auth={urllib.parse.quote(FRONTEND_AUTH_TOKEN)}"
        with urllib.request.urlopen(url, timeout=0.4) as response:
            return response.status == 204
    except Exception:
        return False


def main() -> None:
    url = f"http://{HOST}:{PORT}/?auth={urllib.parse.quote(FRONTEND_AUTH_TOKEN)}"
    if already_running():
        webbrowser.open(url)
        return
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    if "--no-browser" not in sys.argv:
        threading.Timer(0.45, lambda: webbrowser.open(url)).start()
    server.serve_forever()


if __name__ == "__main__":
    main()



