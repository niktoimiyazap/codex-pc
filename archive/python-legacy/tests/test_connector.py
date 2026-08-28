from __future__ import annotations

import base64
import tempfile
import unittest
from pathlib import Path
from typing import Any

from codexpc_connector.config import Settings
from codexpc_connector.tools import CodexTools, connector_tools, json_text


class FakeAppServer:
    def __init__(self) -> None:
        self.running = True
        self.calls: list[tuple[str, dict[str, Any]]] = []

    async def request(self, method: str, params: dict[str, Any], **_: Any) -> Any:
        self.calls.append((method, params))
        if method == "fs/readFile":
            return {"dataBase64": base64.b64encode("РџСЂРёРІРµС‚\n".encode()).decode("ascii")}
        if method == "command/exec":
            command = [str(value) for value in params.get("command", [])]
            if command and command[-1].endswith(".mjs"):
                return {
                    "exitCode": 0,
                    "stdout": (
                        '{"ok":true,"result":{"final_url":"https://example.com/",'
                        '"title":"Example Domain","h1":"Example Domain"}}\n'
                    ),
                    "stderr": "",
                }
            return {"exitCode": 0, "stdout": "ok\n", "stderr": ""}
        if method == "mcpServer/tool/call":
            return {"content": [{"type": "text", "text": "ok"}], "isError": False}
        return {}

    async def ensure_thread(self, session_key: str = "default") -> str:
        return f"thread-{session_key}"

    async def run_turn(self, thread_id: str, instruction: str, **_: Any) -> Any:
        self.calls.append(("turn/start+collect", {"threadId": thread_id, "instruction": instruction}))
        return {"text": '{"ok":true,"result":{"tabs":[]},"error":null}', "turn": {"id": "turn-1"}}


class ConnectorTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.settings = Settings(
            state_dir=self.root / "state",
            workspace=self.root,
            allowed_roots=[self.root],
            default_tool_timeout_sec=5,
            max_output_chars=20_000,
        )
        self.client = FakeAppServer()
        self.tools = CodexTools(self.settings, self.client)  # type: ignore[arg-type]

    async def asyncTearDown(self) -> None:
        await self.tools.shutdown()
        self.temp.cleanup()

    def test_public_surface_is_thin(self) -> None:
        names = {tool.name for tool in connector_tools(self.settings)}
        self.assertEqual(
            names,
            {
                "connector_status",
                "fs_read_file",
                "fs_write_file",
                "fs_edit_file",
                "fs_read_directory",
                "fs_create_directory",
                "fs_copy",
                "fs_remove",
                "command_exec",
                "computer",
                "mcp_discover",
                "mcp_call",
            },
        )
        self.assertNotIn("edit_file", names)
        self.assertNotIn("run_command", names)
        self.assertNotIn("run_process", names)
        self.assertNotIn("project_overview", names)

    async def test_read_routes_to_original_codex(self) -> None:
        target = self.root / "demo.txt"
        result = await self.tools.call("fs_read_file", {"path": str(target)})
        self.assertEqual(result["content"], "РџСЂРёРІРµС‚\n")
        self.assertEqual(self.client.calls, [("fs/readFile", {"path": str(target)})])

    async def test_write_routes_to_original_codex(self) -> None:
        target = self.root / "demo.txt"
        result = await self.tools.call(
            "fs_write_file",
            {"path": str(target), "content": "ж—Ґжњ¬иЄћ вњ…"},
        )
        method, params = self.client.calls[-1]
        self.assertEqual(method, "fs/writeFile")
        self.assertEqual(base64.b64decode(params["dataBase64"]).decode(), "ж—Ґжњ¬иЄћ вњ…")
        self.assertTrue(result["written"])

    async def test_edit_file_replaces_exact_fragment_and_preserves_crlf(self) -> None:
        target = self.root / "edit.txt"
        target.write_bytes(b"alpha\r\nbeta\r\n")
        import hashlib

        expected = hashlib.sha256(target.read_bytes()).hexdigest()
        result = await self.tools.call(
            "fs_edit_file",
            {
                "path": str(target),
                "expected_sha256": expected,
                "edits": [{"old_text": "beta", "new_text": "gamma"}],
            },
        )
        self.assertEqual(target.read_bytes(), b"alpha\r\ngamma\r\n")
        self.assertEqual(result["newline"], "crlf")
        self.assertEqual(result["replacements"], 1)
        self.assertIn("-beta", result["diff"])
        self.assertIn("+gamma", result["diff"])

    async def test_edit_file_rejects_stale_hash(self) -> None:
        target = self.root / "stale.txt"
        target.write_text("one", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "hash does not match"):
            await self.tools.call(
                "fs_edit_file",
                {
                    "path": str(target),
                    "expected_sha256": "0" * 64,
                    "edits": [{"old_text": "one", "new_text": "two"}],
                },
            )
        self.assertEqual(target.read_text(encoding="utf-8"), "one")

    async def test_edit_file_rejects_ambiguous_match(self) -> None:
        target = self.root / "ambiguous.txt"
        target.write_text("x x", encoding="utf-8")
        import hashlib

        expected = hashlib.sha256(target.read_bytes()).hexdigest()
        with self.assertRaisesRegex(ValueError, "expected 1 match"):
            await self.tools.call(
                "fs_edit_file",
                {
                    "path": str(target),
                    "expected_sha256": expected,
                    "edits": [{"old_text": "x", "new_text": "y"}],
                },
            )
        self.assertEqual(target.read_text(encoding="utf-8"), "x x")

    async def test_directory_operations_route_to_original_codex(self) -> None:
        source = self.root / "source"
        destination = self.root / "destination"
        await self.tools.call("fs_read_directory", {"path": str(source)})
        await self.tools.call("fs_create_directory", {"path": str(destination)})
        await self.tools.call(
            "fs_copy",
            {
                "source_path": str(source),
                "destination_path": str(destination),
                "recursive": True,
            },
        )
        await self.tools.call("fs_remove", {"path": str(destination), "recursive": True})
        self.assertEqual(
            [method for method, _ in self.client.calls],
            ["fs/readDirectory", "fs/createDirectory", "fs/copy", "fs/remove"],
        )

    async def test_terminal_routes_to_original_codex(self) -> None:
        result = await self.tools.call(
            "command_exec",
            {
                "command": ["python", "-c", "print('ok')"],
                "cwd": str(self.root),
                "timeout_ms": 5000,
            },
        )
        method, params = self.client.calls[-1]
        self.assertEqual(method, "command/exec")
        self.assertEqual(params["command"], ["python", "-c", "print('ok')"])
        self.assertEqual(params["cwd"], str(self.root))
        self.assertEqual(result["exitCode"], 0)

    async def test_path_policy_still_guards_original_codex_calls(self) -> None:
        outside = self.root.parent / "outside.txt"
        with self.assertRaises(PermissionError):
            await self.tools.call("fs_read_file", {"path": str(outside)})
        self.assertEqual(self.client.calls, [])

    async def test_mcp_call_still_uses_codex_app_server(self) -> None:
        result = await self.tools.call(
            "mcp_call",
            {"server_name": "demo", "tool_name": "ping", "arguments": {"value": 1}},
            session_key="chat-a",
        )
        method, params = self.client.calls[-1]
        self.assertEqual(method, "mcpServer/tool/call")
        self.assertEqual(params["threadId"], "thread-chat-a")
        self.assertFalse(result["isError"])

    async def test_status_reports_codex_backend(self) -> None:
        result = await self.tools.call("connector_status", {})
        self.assertEqual(result["execution_backend"], "codex app-server")
        self.assertTrue(result["app_server_running"])

    def test_json_text_is_bounded(self) -> None:
        self.assertEqual(json_text({"ok": True}, 100), '{\n  "ok": true\n}')
        self.assertTrue(json_text("x" * 20, 5).endswith("... output truncated ..."))


if __name__ == "__main__":
    unittest.main()
