<div align="center">

# CodexPC

**A local control plane that lets ChatGPT work on your Windows PC through MCP — without turning your machine into a pile of shell scripts.**

[Русский](README.ru.md) · [Setup](docs/TUNNEL_SETUP.md) · [Architecture](docs/ARCHITECTURE.md) · [Security](SECURITY.md)

[![Windows](https://img.shields.io/badge/Windows-first-0078D4?logo=windows11&logoColor=white)](#quick-start)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![MCP](https://img.shields.io/badge/Protocol-MCP-111827)](https://modelcontextprotocol.io/)
[![CI](https://img.shields.io/github/actions/workflow/status/niktoimiyazap/codex-mcp-router/test.yml?branch=main&label=tests)](https://github.com/niktoimiyazap/codex-mcp-router/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-22C55E.svg)](LICENSE)

<br>

<img src="assets/screenshots/codex-pc-ui.png" alt="CodexPC local frontend" width="100%">

<br>

<img src="assets/screenshots/codex-pc-ui-start.png" alt="CodexPC first-run setup" width="100%">

</div>

```text
                 OpenAI Tunnel
ChatGPT  ─────────────────────────►  CodexPC
                                        │
                 ┌──────────────────────┼──────────────────────┐
                 │                      │                      │
                 ▼                      ▼                      ▼
          Codex app-server       Native Windows          Local frontend
          files · MCP · rules    commands · desktop      sessions · setup
```

CodexPC is a local MCP bridge built around the original Codex app-server. It gives ChatGPT a structured way to inspect projects, edit files, run real development workflows, control the Windows desktop, call other MCP servers, keep long-running commands alive, and ask for human approval when sensitive credentials are involved.

The important part: **the model gets tools, not a giant unrestricted shell prompt.**

## Why CodexPC

| | |
| --- | --- |
| **One install** | `install.cmd` prepares the required Go toolchain, Python frontend runtime, Codex CLI, official `tunnel-client`, dependencies, build and smoke test. |
| **Real Codex underneath** | Filesystem operations, rules and downstream MCP integrations reuse the original Codex app-server instead of reimplementing everything badly. |
| **Long-running work actually works** | Native Windows command sessions can continue past one MCP request and can be polled, written to or terminated later. |
| **Human-in-the-loop secrets** | Credentials stay in the local vault. Sensitive command injection and direct secret inspection require explicit approval. |
| **Useful local UI** | Named chat sessions, tool activity, history, background processes, approvals, errors, secrets and setup live in one local frontend. |
| **Windows-native control** | Screenshots, mouse, keyboard and scrolling are first-class tools rather than shell hacks. |

## Quick start

### 1. Download CodexPC

Clone the repository or download it as a ZIP:

```powershell
git clone https://github.com/niktoimiyazap/codex-mcp-router.git
cd codex-mcp-router
```

### 2. Run the installer

Double-click `install.cmd`, or run:

```bat
install.cmd
```

The installer is idempotent: if a dependency is already installed and compatible, CodexPC reuses it instead of reinstalling it.

It prepares:

- the Go version required by `go.mod`;
- Python for the local frontend;
- Codex CLI and its login state;
- the official OpenAI `tunnel-client`;
- Go modules, tests, the production build and smoke verification.

### 3. Create an OpenAI Secure MCP Tunnel

Open [Platform → Organization → Tunnels](https://platform.openai.com/settings/organization/tunnels) and create a tunnel for CodexPC.

You need:

- **Tunnels: Read + Manage** to create or edit a tunnel;
- **Tunnels: Read + Use** for the account/service that will run the tunnel and attach it in ChatGPT.

When creating the tunnel, associate it with the **ChatGPT workspace that will use CodexPC**. After creation, copy its ID — it looks like:

```text
tunnel_0123456789abcdef0123456789abcdef
```

Then create a **Restricted runtime API key** in [Platform → Organization → API keys](https://platform.openai.com/settings/organization/api-keys) with **Tunnels: Read + Use**. This is the `CONTROL_PLANE_API_KEY` used by `tunnel-client`; it is not an admin key and is not pasted into ChatGPT.

See [First-run & tunnel setup](docs/TUNNEL_SETUP.md) for the full permissions and troubleshooting flow.

### 4. Finish setup in the local UI

After installation, CodexPC opens its local setup page. Choose:

- your default workspace;
- `core` or `full` tool profile;
- the OpenAI Tunnel ID from the previous step;
- the restricted runtime API key;
- optional tunnel profile / organization labels.

Non-secret values are written to the normal CodexPC config. The runtime key is encrypted for the current Windows account with **DPAPI** and is never stored in TOML or browser LocalStorage.

Before saving a new tunnel configuration, CodexPC validates it in an isolated temporary profile with `tunnel-client doctor`. A bad key or Tunnel ID does not overwrite a working setup.

### 5. Connect CodexPC in ChatGPT

Keep CodexPC running, then open ChatGPT on the web and enable **Developer mode** for your account/workspace if required by your plan.

Create a custom app from **Settings → Apps → Create** (or **Workspace settings → Apps → Create** for workspace admins):

1. set **Connection** to **Tunnel**;
2. select the CodexPC tunnel, or paste its `tunnel_...` ID;
3. click **Scan Tools** and wait for CodexPC tools to appear;
4. click **Create**;
5. select the new CodexPC app when sending a message in ChatGPT.

The runtime API key stays on your PC. ChatGPT only needs access to the tunnel object.

OpenAI currently exposes full custom MCP write/modify actions to Business and Enterprise/Edu workspaces; Pro can use custom MCP apps in developer mode with read/fetch permissions. Product UI and availability may change while MCP support is in beta.

### 6. Daily launch

After first setup, use:

```bat
start.cmd
```

The launcher shows the short CodexPC terminal intro, starts the local frontend, restores the configured runtime and keeps the tunnel supervised.

## What ChatGPT gets

CodexPC intentionally exposes purpose-built tools instead of one monolithic command interface.

| Area | Examples |
| --- | --- |
| **Sessions & project rules** | `session_create`, `session_list`, `read_rules` |
| **Filesystem** | `fs_read_file`, `fs_edit_file`, `fs_write_file`, `fs_search`, `fs_copy`, `fs_remove` |
| **Terminal** | `command_inspect`, `command_exec`, `shell_exec`, `command_poll`, `command_write`, `command_terminate` |
| **Desktop** | `computer` — screenshots, mouse, keyboard, scrolling |
| **MCP routing** | `mcp_discover`, `mcp_call`, `mcp_resource_read`, `mcp_reload`, `mcp_oauth_login` |
| **Secrets** | `secret_vault`, approval-gated `credential_value`, credential references for commands |
| **Control plane** | `connector_status`, emergency process control, `multi_tool` batching |

The default `core` profile keeps the surface focused. `full` exposes additional compatibility and diagnostic tools.

## How it works

```text
start.cmd
   │
   └─ scripts/start-codexpc.ps1
          ├─ frontend/server.pyw ───────► http://127.0.0.1:8765
          └─ tunnel-client
                 │ MCP over stdio
                 ▼
           dist/codexpc-go.exe
                 │
                 ├─ Codex app-server ───► fs / rules / configured MCP servers
                 ├─ native command supervisor
                 ├─ Windows desktop control
                 └─ structured local state + logs
```

CodexPC keeps one long-lived Codex app-server connection and layers session ownership, path policy, MCP inventory caching, response normalization, process supervision, approvals and the local frontend around it.

For the full boundary and request flow, see [Architecture](docs/ARCHITECTURE.md).

## Security model

CodexPC is privileged local software, so the design assumes **one trusted Windows user** and keeps the trust boundary local.

- The frontend binds to loopback (`127.0.0.1`) and uses a local auth bootstrap cookie.
- Filesystem paths are checked against configured `allowed_roots` before privileged operations.
- The tunnel runtime key is protected with Windows DPAPI.
- Secret values are represented by opaque credential references whenever possible.
- Sensitive credential use can require explicit approval in the frontend.
- Logs and returned metadata are redacted and bounded.
- Tunnel configuration is validated before it replaces the active setup.

Do not expose the connector or its local frontend directly to a public network. Read [SECURITY.md](SECURITY.md) before extending the trust boundary.

## Configuration

The preferred configuration path is the setup UI. The generated per-user state lives by default at:

```text
%LOCALAPPDATA%\CodexPCConnector
```

The non-secret configuration is stored in `config.toml`. A minimal manual example is available in [`config.example.toml`](config.example.toml):

```toml
workspace = "C:/Users/you/projects"
allowed_roots = ["C:/Users/you/projects"]
tool_profile = "core"
```

Set `CODEXPC_STATE_DIR` if you need to move the whole state directory. See [Configuration](docs/CONFIGURATION.md) for the supported keys and launcher overrides.

## Repository layout

```text
codex-mcp-router/
├─ cmd/codexpc/        Go entry point
├─ internal/           connector core, app-server client, MCP, security, Windows control
├─ frontend/           local setup + activity UI and its loopback server
├─ scripts/            install, start, build and supervision internals
├─ docs/               architecture, configuration and setup documentation
├─ install.cmd         first install / repair entry point
├─ start.cmd           normal user launcher
├─ config.example.toml manual configuration reference
└─ go.mod
```

`dist/` and `.local/` are generated runtime/build directories and are intentionally ignored by Git.

## Building from source

For development on Windows:

```bat
scripts\build.cmd -NoDesktopCopy
```

The build pipeline runs formatting, `go test ./...`, a production build, the real app-server smoke test and staged deployment into `dist/`.

Or run the Go steps manually:

```powershell
go fmt ./cmd/... ./internal/...
go test ./...
go build -trimpath -o dist/codexpc-go.exe ./cmd/codexpc
```

## Documentation

- [Documentation index](docs/README.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Configuration](docs/CONFIGURATION.md)
- [First-run & tunnel setup](docs/TUNNEL_SETUP.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)

## License

CodexPC is released under the [MIT License](LICENSE).
