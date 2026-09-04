# Configuration

The setup UI is the preferred way to configure CodexPC. It writes the same per-user TOML file the Go connector reads, so there is no second frontend-only configuration system.

## State directory

Default on Windows:

```text
%LOCALAPPDATA%\CodexPCConnector
```

Set `CODEXPC_STATE_DIR` to move the entire state directory. This also moves configuration, logs, sessions, locks, frontend auth state and DPAPI-protected CodexPC secrets.

The non-secret configuration file is:

```text
<state-dir>\config.toml
```

## Example

```toml
workspace = "C:/Users/you/projects"
allowed_roots = ["C:/Users/you/projects"]

default_startup_timeout_sec = 45
default_tool_timeout_sec = 120
max_output_chars = 100000
mcp_inventory_ttl_sec = 300

tool_profile = "core"

tunnel_profile = "codex"
tunnel_id = "tunnel_0123456789abcdef0123456789abcdef"
organization = ""

log_level = "INFO"
```

The runtime API key is deliberately absent. On Windows the setup UI stores it separately using DPAPI for the current Windows account.

## Supported keys

### `workspace`

Default working directory for project operations and relative command paths.

Default: the current user's home directory.

### `allowed_roots`

Filesystem roots CodexPC is allowed to access. Use the narrowest roots that cover the intended work.

Default: the current user's home directory.

### `default_startup_timeout_sec`

Startup budget used for connector/app-server initialization paths.

Default: `45`.

### `default_tool_timeout_sec`

Default timeout used by tool operations that need a request-level budget and do not provide a more specific value.

Default: `120`.

Long-running Windows commands are session-oriented and can continue after the initial MCP call returns a running process handle.

### `max_output_chars`

Maximum normalized output size returned by the connector for bounded tool results.

Default: `100000`.

### `mcp_inventory_ttl_sec`

How long a downstream MCP inventory remains fresh before CodexPC refreshes it.

Default: `300`.

### `tool_profile`

Controls the advertised tool surface:

- `core` — normal production surface with compatibility/diagnostic noise hidden;
- `full` — compatibility and additional diagnostic tools are also advertised.

Default: `core`.

### `tunnel_profile`

Local `tunnel-client` profile name used by the Windows start wrapper.

Default: `codex`.

### `tunnel_id`

OpenAI Tunnel ID used by the runtime. The setup UI validates its format and tunnel configuration before committing changes.

### `organization`

Optional local organization label stored with the CodexPC setup metadata.

### `log_level`

Structured connector log level.

Default: `INFO`.

## Connector environment overrides

The Go config loader supports these direct overrides:

| Variable | Purpose |
| --- | --- |
| `CODEXPC_STATE_DIR` | Move the complete state directory |
| `CODEXPC_WORKSPACE` | Override `workspace` |
| `CODEXPC_ALLOWED_ROOTS` | Override `allowed_roots` using the platform path separator |
| `CODEXPC_TOOL_PROFILE` | Override `tool_profile` (`core` or `full`) |

## Windows launcher overrides

The installer/start scripts also recognize a few operational variables:

| Variable | Purpose |
| --- | --- |
| `TUNNEL_CLIENT_PATH` | Explicit path to `tunnel-client.exe` |
| `CODEXPC_PYTHONW_PATH` | Explicit Python GUI runtime for `frontend/server.pyw` |
| `CODEXPC_NO_INTRO=1` | Skip the animated terminal intro in `start.cmd` |

These are launcher settings, not normal application configuration.

## Manual source checkout

For normal users, `install.cmd` creates and validates the tunnel profile automatically.

For direct local MCP testing from a source checkout, the stdio command can point to:

```text
scripts\wrapper.cmd
```

That wrapper simply launches the current `dist\codexpc-go.exe` and fails clearly when the binary has not been built yet.

Build it with:

```bat
scripts\build.cmd -NoDesktopCopy
```

## Troubleshooting

### Setup keeps opening

The configuration is not considered complete until a valid Tunnel ID, saved runtime key and successful tunnel validation are present. Open `http://127.0.0.1:8765/setup/` and finish the setup flow.

### A path is denied

Add the narrowest appropriate parent directory to `allowed_roots`. Paths are resolved before authorization.

### The frontend cannot start

Run `install.cmd` again. It repairs missing runtime dependencies and persists the discovered Python runtime path.

### The tunnel cannot start

Open Setup & settings and validate the Tunnel ID/runtime key again. CodexPC tests new tunnel data before replacing the active profile.

### A second connector instance fails

Only one connector may own a state directory at a time. Use a different `CODEXPC_STATE_DIR` only when you intentionally want an isolated second runtime.
