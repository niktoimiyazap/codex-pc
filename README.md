# CodexPC Connector

[English](README.md) | [Русский](README.ru.md)

> A thin MCP stdio adapter over the original Codex app-server, with downstream MCP routing and native Windows desktop control.

[![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![MCP](https://img.shields.io/badge/Protocol-MCP-111827)](https://modelcontextprotocol.io/)
[![License: MIT](https://img.shields.io/badge/License-MIT-22C55E.svg)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/niktoimiyazap/codex-mcp-router/test.yml?branch=main&label=tests)](https://github.com/niktoimiyazap/codex-mcp-router/actions)

## What it does

CodexPC Connector exposes a controlled local tool layer to MCP clients:

- guarded filesystem reads and mutations;
- atomic UTF-8 writes with conflict protection;
- synchronous and background process execution;
- timeouts, cancellation, bounded output, and process-tree termination;
- downstream MCP inventory, search, and calls through Codex app-server;
- secret-redacted structured logs and single-instance protection.

## Architecture

```text
MCP client
    |
    v
CodexPC Connector
    |-- filesystem policy and UTF-8 validation
    |-- managed local process jobs
    `-- JSON-RPC / JSONL client
             |
             v
       codex app-server
         |-- fs/*
         |-- mcpServerStatus/list
         `-- mcpServer/tool/call
```

The connector starts one long-lived `codex app-server --stdio` process and creates one ephemeral Codex thread for MCP discovery and calls.

## Requirements

- Go 1.26 or newer (only required to build from source);
- Codex CLI with `codex app-server` support;
- an authenticated Codex installation;
- MCP servers configured through Codex when downstream routing is needed.

## Quick start

```bash
git clone https://github.com/niktoimiyazap/codex-mcp-router.git
cd codex-mcp-router
go build -o dist/codexpc-go.exe ./cmd/codexpc
```

Windows recommended build and run:

```bat
build-go.cmd
wrapper.cmd
```

`build-go.cmd` runs formatting, tests, build, a smoke test, deploys `dist\codexpc-go.exe`, and copies the binary to the Desktop when possible.

The former Python runtime is preserved only under `archive/python-legacy/` for historical reference and regression comparison.

## Interactive tunnel launcher

Connect this local MCP server to an existing OpenAI tunnel without placing API keys in files:

```bat
launch-tunnel.cmd
```

```bash
chmod +x launch-tunnel.sh
./launch-tunnel.sh
```

The launcher asks for the tunnel details once and stores the runtime API key in Windows Credential Manager or macOS Keychain. Later launches reuse it automatically. See [Interactive tunnel setup](docs/TUNNEL_SETUP.md).

## Configuration

Copy `config.example.toml` to the platform state directory:

| Platform | Configuration path |
|---|---|
| Windows | `%LOCALAPPDATA%\CodexPCConnector\config.toml` |
| macOS | `~/Library/Application Support/CodexPCConnector/config.toml` |
| Linux | `$XDG_STATE_HOME/codexpc-connector/config.toml` |

Minimal example:

```toml
workspace = "~/projects"
allowed_roots = ["~/projects"]

tool_profile = "core"
```

The connector is intentionally thin. Filesystem and terminal operations are delegated to the original Codex app-server. The connector keeps path-policy checks, MCP routing, response normalization, and native Windows desktop control.

See [Configuration](docs/CONFIGURATION.md) for all options.

## Tool groups

### Filesystem

`fs_read_file`, `fs_write_file`, `fs_read_directory`, `fs_create_directory`, `fs_copy`, `fs_remove`

These tools are validated adapters over the original Codex `fs/*` methods. The connector no longer contains a patch engine, file snapshot cache, project walker, or direct filesystem implementation.

### Terminal

`command_exec`

Commands are passed as argv vectors to the original Codex `command/exec` method. The connector no longer owns subprocesses or a background job manager.

### Desktop control

`computer`

Native Windows screenshot, mouse, keyboard, and scrolling support remains local.

### MCP routing

`mcp_discover`, `mcp_call`

`mcp_discover` lists or searches configured MCP servers and tools from one shared persistent inventory cache. Stale data is returned immediately while a background refresh updates it. The `full` profile additionally exposes `mcp_list_servers`, `mcp_list_tools`, and `mcp_search_tools` as compatibility aliases.

### Connector control

`connector_status`, `list_active_tool_calls`, `cancel_tool_calls`

## Verification

```bash
go fmt ./cmd/... ./internal/...
go test ./...
go build -trimpath -o dist/codexpc-go.exe ./cmd/codexpc
```

On Windows the complete verification pipeline is:

```bat
build-go.cmd
```

## Documentation

- [Architecture](docs/ARCHITECTURE.md)
- [Configuration](docs/CONFIGURATION.md)
- [Interactive tunnel setup](docs/TUNNEL_SETUP.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)
- [Release process](docs/RELEASING.md)
- [Changelog](CHANGELOG.md)

## Security

This is privileged local software intended for a single trusted user over MCP stdio. Do not expose it directly to a public network. Review [SECURITY.md](SECURITY.md) before enabling process or shell execution.

## License

MIT
