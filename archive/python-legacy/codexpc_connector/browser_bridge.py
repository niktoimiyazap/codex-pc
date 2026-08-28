from __future__ import annotations

import json
import os
import re
import tempfile
from pathlib import Path
from typing import Any

from .app_server import AppServerError, CodexAppServerClient

_URL_RE = re.compile(r"https?://[^\s\"'<>]+", re.IGNORECASE)


class BundledBrowserBridge:
    """Runs deterministic navigation through Codex's bundled @oai/sky runtime."""

    def __init__(self, client: CodexAppServerClient) -> None:
        self.client = client

    async def call(self, session_key: str, task: str, *, timeout_sec: float | None = None) -> Any:
        del session_key
        match = _URL_RE.search(task)
        if not match:
            raise AppServerError("browser_use currently requires an explicit http:// or https:// URL")
        url = match.group(0).rstrip(".,);]")
        runtime = self._runtime()
        node = runtime / "bin" / "node.exe"
        sky_module = (
            runtime
            / "bin"
            / "node_modules"
            / "@oai"
            / "sky"
            / "dist"
            / "project"
            / "cua"
            / "sky_js"
            / "src"
            / "index.js"
        )
        if not node.is_file() or not sky_module.is_file():
            raise AppServerError("Codex bundled CUA runtime is incomplete")

        temp_path: Path | None = None
        try:
            with tempfile.NamedTemporaryFile("w", suffix=".mjs", encoding="utf-8", delete=False) as handle:
                handle.write(self._worker_script(sky_module.as_uri(), url))
                temp_path = Path(handle.name)
            effective_timeout = float(timeout_sec or 45.0)
            result = await self.client.request(
                "command/exec",
                {
                    "command": [str(node), str(temp_path)],
                    "cwd": str(getattr(self.client, "workspace", Path.cwd())),
                    "timeoutMs": max(1000, int(effective_timeout * 1000)),
                    "outputBytesCap": 2_000_000,
                    "env": {
                        "NODE_REPL_REQUEST_META": json.dumps(
                            {"x-oai-cua-approved-app": "Chrome"}, separators=(",", ":")
                        )
                    },
                },
                timeout_sec=effective_timeout + 5,
            )
        finally:
            if temp_path is not None:
                temp_path.unlink(missing_ok=True)

        if int(result.get("exitCode", 1)) != 0:
            error = str(result.get("stderr") or result.get("stdout") or "Bundled browser worker failed").strip()
            raise AppServerError(error[:2000])
        stdout = str(result.get("stdout") or "").strip()
        try:
            payload = json.loads(stdout)
        except json.JSONDecodeError as exc:
            raise AppServerError(f"Bundled browser returned invalid JSON: {stdout[:1000]}") from exc
        if not payload.get("ok"):
            raise AppServerError(str(payload.get("error") or "Bundled browser task failed"))
        return payload

    @staticmethod
    def _runtime() -> Path:
        root = Path(os.environ.get("LOCALAPPDATA", "")) / "OpenAI" / "Codex" / "runtimes" / "cua_node"
        candidates = [path for path in root.iterdir() if path.is_dir()] if root.is_dir() else []
        if not candidates:
            raise AppServerError("Codex bundled CUA runtime was not found")
        return max(candidates, key=lambda path: path.stat().st_mtime)

    @staticmethod
    def _worker_script(sky_url: str, url: str) -> str:
        template = r'''import { execFileSync } from "node:child_process";
const { sky } = await import(__SKY_URL__);
const targetUrl = __TARGET_URL__;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const clipboard = () => execFileSync(
  "powershell.exe",
  [
    "-NoProfile",
    "-NonInteractive",
    "-Command",
    "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); Get-Clipboard -Raw"
  ],
  { encoding: "utf8", windowsHide: true }
).replace(/\r\n/g, "\n").trim();

try {
  let apps = await sky.list_apps();
  let app = apps.find((value) => /chrome/i.test(value.id) || /chrome/i.test(value.displayName || ""));
  if (!app) {
    app = apps.find(
      (value) => /edge|chromium/i.test(value.id) || /edge|chromium/i.test(value.displayName || "")
    );
  }
  if (!app) throw new Error("No supported browser application was found");
  if (!app.windows?.length) {
    await sky.launch_app({ app: app.id });
    await sleep(1500);
    apps = await sky.list_apps();
    app = apps.find((value) => value.id === app.id);
  }
  const window = app?.windows?.[0];
  if (!window) throw new Error("No browser window is available");

  const readAddressBar = async () => {
    await sky.activate_window({ window });
    await sleep(120);
    await sky.press_key({ window, key: "Ctrl+L" });
    await sleep(120);
    await sky.press_key({ window, key: "Ctrl+A" });
    await sky.press_key({ window, key: "Ctrl+C" });
    await sleep(220);
    return clipboard();
  };
  const navigate = async (destination, { newTab = false } = {}) => {
    const expected = new URL(destination);
    let lastAddress = "";
    for (let attempt = 0; attempt < 4; attempt += 1) {
      await sky.activate_window({ window });
      await sleep(120);
      await sky.press_key({ window, key: "Ctrl+L" });
      await sleep(120);
      await sky.press_key({ window, key: "Ctrl+A" });
      await sky.press_key({ window, key: "BackSpace" });
      await sky.type_text({ window, text: destination });
      await sleep(120);
      await sky.press_key({ window, key: newTab ? "Alt+Return" : "Return" });
      await sleep(1600);
      newTab = false;
      lastAddress = await readAddressBar();
      try {
        const current = new URL(lastAddress);
        if (current.origin === expected.origin) {
          await sky.press_key({ window, key: "Escape" });
          await sleep(120);
          return current.href;
        }
      } catch {}
      await sky.press_key({ window, key: "Escape" });
    }
    throw new Error(`Browser navigated to unexpected origin: ${lastAddress || "unknown"}`);
  };

  const finalUrl = await navigate(targetUrl, { newTab: true });

  let source = "";
  try {
    const response = await fetch(finalUrl, {
      redirect: "follow",
      headers: {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131 Safari/537.36",
        "Accept-Language": "en-US,en;q=0.9"
      }
    });
    source = await response.text();
  } catch {}
  const decode = (value) => value
    .replace(/<[^>]+>/g, " ")
    .replace(/&nbsp;/gi, " ")
    .replace(/&amp;/gi, "&")
    .replace(/&lt;/gi, "<")
    .replace(/&gt;/gi, ">")
    .replace(/&quot;/gi, "\"")
    .replace(/&#39;/gi, "'")
    .replace(/\s+/g, " ")
    .trim();
  const titleMatch = source.match(/<title[^>]*>([\s\S]*?)<\/title>/i);
  const ogTitleMatch = source.match(/<meta[^>]+property=["']og:title["'][^>]+content=["']([^"']*)/i);
  const h1Match = source.match(/<h1[^>]*>([\s\S]*?)<\/h1>/i);
  const title = decode(titleMatch?.[1] || ogTitleMatch?.[1] || "");
  const heading = decode(h1Match?.[1] || "");
  console.log(JSON.stringify({ ok: true, result: { final_url: finalUrl, title, h1: heading } }));
} catch (error) {
  console.log(JSON.stringify({ ok: false, error: String(error?.message || error) }));
}
'''
        return template.replace("__SKY_URL__", json.dumps(sky_url)).replace("__TARGET_URL__", json.dumps(url))

