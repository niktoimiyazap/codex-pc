# Configuration

CodexPC Connector loads one per-user TOML file. Environment variables may override the workspace, allowed roots, and capability switches.

## Configuration file locations

| Platform | Path |
| --- | --- |
| Windows | `%LOCALAPPDATA%\CodexPCConnector\config.toml` |
| macOS | `~/Library/Application Support/CodexPCConnector/config.toml` |
| Linux | `$XDG_STATE_HOME/codexpc-connector/config.toml` or `~/.local/state/codexpc-connector/config.toml` |

Set `CODEXPC_STATE_DIR` to move the entire state directory, including configuration, logs, and the single-instance lock.

## Minimal configuration

```toml
workspace = "~/projects"
allowed_roots = ["~/projects"]

enable_process = false
enable_shell = false
enable_delete = true
```

Copy `config.example.toml` as a starting point. Keep real configuration outside the repository.

## Options

### `workspace`

Working directory used when starting `codex app-server` and the default context for relative process paths.

Default: the current user's home directory.

### `allowed_roots`

List of filesystem roots the connector may access. Paths are expanded and resolved before authorization.

Default: the current user's home directory.

Use the narrowest roots that cover the intended work. Do not add an entire system drive unless the connector genuinely requires it.

### `enable_process`

Enables direct program execution through `run_process` and the managed job tools.

Default: `false`.

### `enable_shell`

Enables shell command strings through `run_command`. This requires `enable_process=true` as well.

Default: `false`.

Prefer `run_process` with an argument vector when possible. It avoids shell quoting and interpolation hazards.

### `enable_delete`

Enables file and directory deletion after path authorization.

Default: `true`.

Set it to `false` for read/write-only deployments.

### `default_tool_timeout_sec`

Default timeout for app-server JSON-RPC calls and local process execution when a call does not provide a more specific timeout.

Default: `120`.

### `max_output_chars`

Maximum serialized result size returned to the MCP client.

Default: `100000`.

### `max_read_chars`

Maximum number of decoded text characters returned by `read_file`.

Default: `500000`.

### `max_job_history`

Maximum number of managed process jobs retained for inspection.

Default: `100`; minimum effective value: `10`.

### `mcp_inventory_ttl_sec`

Number of seconds for which the shared downstream MCP inventory is considered fresh. Stale disk-cached data remains immediately available while a background refresh runs.

Default: `300`; minimum effective value: `1`.

### `tool_profile`

Controls which tools are advertised to the MCP client:

- `core` exposes the normal development workflow and hides duplicate compatibility aliases, desktop `SendKeys`, and connector diagnostics.
- `full` exposes every compatibility and diagnostic tool.

Hidden compatibility handlers remain implemented, but new clients do not receive their schemas in `core` mode.

Default: `core`.

### `log_level`

Structured connector log level.

Default: `INFO`.

## Environment overrides

| Variable | Purpose |
| --- | --- |
| `CODEXPC_STATE_DIR` | Override the state directory |
| `CODEXPC_WORKSPACE` | Override `workspace` |
| `CODEXPC_ALLOWED_ROOTS` | Override `allowed_roots`; use the platform path separator |
| `CODEXPC_ENABLE_PROCESS` | Override `enable_process` |
| `CODEXPC_ENABLE_SHELL` | Override `enable_shell` |
| `CODEXPC_ENABLE_DELETE` | Override `enable_delete` |
| `CODEXPC_TOOL_PROFILE` | Override `tool_profile` with `core` or `full` |

Boolean values accept `1`, `true`, `yes`, or `on` as true; other values are false.

## MCP client example

The exact format depends on the MCP host. A typical stdio entry points to the installed command:

```json
{
  "mcpServers": {
    "CodexPC": {
      "command": "codexpc-connector",
      "args": []
    }
  }
}
```

For a source checkout on Windows, the command may instead point to `wrapper.cmd` using an absolute path.

## Recommended profiles

### Filesystem-only

```toml
workspace = "~/projects"
allowed_roots = ["~/projects"]
enable_process = false
enable_shell = false
enable_delete = false
```

### Development workstation

```toml
workspace = "~/projects"
allowed_roots = ["~/projects"]
enable_process = true
enable_shell = true
enable_delete = true
default_tool_timeout_sec = 180
```

Shell and process access are privileged. Enable them only for a trusted local MCP client.

## Troubleshooting

### `Codex CLI was not found in PATH`

Install or update Codex CLI and verify that `codex --version` works in the same environment used by the MCP host.

### Access denied for a path

Add the narrowest appropriate parent directory to `allowed_roots`, then restart the connector. Relative paths are resolved before authorization, so verify the effective workspace as well.

### Process or shell execution is disabled

Set `enable_process=true`. For `run_command`, also set `enable_shell=true`, then restart the connector.

### A second connector instance fails to start

Only one process may use the same state directory. Stop the existing instance or assign a separate `CODEXPC_STATE_DIR`.

### Text appears corrupted

Use `write_file` for text writes and explicitly select `utf-8` when reading uncertain files. The repository self-check rejects non-UTF-8 project text and common mojibake patterns.
