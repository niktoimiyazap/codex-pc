# Changelog

## Unreleased

### Added

- `read_paths`: hybrid batch reading for mixed file and directory targets with recursion, glob filters, deduplication, bounded output, remembered snapshots, and per-path errors.
- `edit_paths`: hybrid batch editing for mixed file and directory targets with atomic per-file writes, optional dry runs, glob filters, snapshot conflict protection, and partial-failure reporting.

## 0.3.0 - 2026-07-16

### Added

- Managed process jobs with PID tracking and explicit `queued`, `running`, `completed`, `failed`, `timed_out`, `cancelled`, and `killed` states.
- `get_job`, `list_jobs`, and `cancel_job` tools.
- Operating-system process-tree termination on Windows and POSIX.
- Per-process timeouts, bounded stdout/stderr capture, and configurable output decoding.
- Atomic UTF-8 text writes with newline control and optional `expected_sha256` conflict protection.
- JSON-RPC request timeouts and cancellation notifications.
- Integration tests for Unicode writes, fast process exit, live background stdout, stale hashes, timeout, cancellation, and full MCP stdio execution.

### Changed

- `run_process` and `run_command` now run synchronously by default; background execution is explicit with `background=true`.
- Windows shell execution now runs locally through a managed process instead of app-server `command/exec`.
- File mutations now call the existing protected-write policy.
- `read_file` decodes before applying the character limit, avoiding truncated multibyte characters.
- Connector shutdown now terminates all managed process jobs before stopping app-server.
- Codex thread permissions remain unchanged: `danger-full-access` with `approvalPolicy=never`.

### Fixed

- Russian text corruption caused by ambiguous shell and PowerShell encodings.
- Windows fast-exit subprocesses remaining `running` after the operating-system process had already exited.
- Child Python processes selecting a legacy Windows code page and failing on Japanese text or emoji output.
- Background jobs disappearing from `list_active_tool_calls` with no alternative visibility.
- Timeout and kill results being reported as successful `completed` jobs with exit code 124.
- Child processes surviving connector cancellation or shutdown.
- The configured default timeout not being applied to JSON-RPC requests.

## 0.2.0 - 2026-07-12

### Changed

- Replaced custom filesystem and process implementations with Codex app-server `fs/*` and `command/exec` RPCs.
- Replaced config parsing and custom downstream MCP workers with `mcpServerStatus/list` and `mcpServer/tool/call`.
- Added a dependency-free asynchronous JSONL client for the official app-server protocol.
- Kept allowed-root enforcement, bounded output, redacted logs, and safe process defaults at the MCP boundary.

## 0.1.0 - 2026-07-10

### Added

- Dynamic MCP discovery from `codex mcp list --json`.
- Lazy stdio and Streamable HTTP MCP workers with idle shutdown.
- Paginated MCP tool listing, search, generic calls, and legacy gateway compatibility.
- File context reading with hashes and atomic line-based diff patches.
- Atomic guarded writes, SHA-256 checked deletion, bounded downstream output, and background task cancellation.
- Allowed-root path policy and protected system write checks.
- Secret-redacted rotating JSONL logs outside the repository.
- Single-instance process lock.
- Cross-platform packaging, unit tests, self-check, and CI.

### Changed

- Removed hardcoded GitHub, Telegram, and Google Drive MCP configuration.
- Removed tools that exposed global instruction or credential files.
- Arbitrary process and shell execution are disabled by default in public configuration.
