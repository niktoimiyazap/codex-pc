# Architecture

CodexPC is a Windows-first local MCP control plane around the original Codex app-server.

The Go connector owns the MCP surface, sessions, path policy, native Windows command supervision, desktop control and downstream routing. The original Codex app-server remains the source of truth for Codex filesystem APIs, project rules and configured MCP integrations.

## Runtime topology

```text
ChatGPT
   │
   │ OpenAI Tunnel
   ▼
tunnel-client
   │
   │ MCP / stdio
   ▼
┌──────────────────────────────────────────────┐
│ dist/codexpc-go.exe                         │
│                                              │
│  internal/mcp       tools, sessions, routing │
│  internal/security  allowed-root policy      │
│  internal/computer  Windows desktop control  │
│  internal/logging   bounded/redacted events  │
│  native supervisor  long-running commands    │
└───────────────────┬──────────────────────────┘
                    │ JSON-RPC / JSONL over stdio
                    ▼
             codex app-server
                    │
         ┌──────────┼──────────┐
         ▼          ▼          ▼
       fs/*      project     configured
                 rules      MCP servers

frontend/server.pyw
   │
   ├─ http://127.0.0.1:8765
   ├─ setup + validation
   ├─ activity / sessions / approvals
   └─ local state and logs
```

## Main components

### `cmd/codexpc/`

The Go entry point. It loads config, acquires the single-instance lock, starts the Codex app-server client, creates the MCP server and watches local restart requests.

### `internal/mcp/`

The main control plane:

- MCP tool schemas and dispatch;
- persistent named chat sessions;
- multi-tool batching;
- downstream MCP discovery, calls, resources, reload and OAuth;
- credential references and approval workflow;
- native Windows command sessions and emergency process control;
- response normalization and bounded output.

### `internal/appserver/`

A long-lived JSON-RPC/JSONL client for the original `codex app-server --stdio` process. It handles request/response correlation and streaming protocol details.

### `internal/security/`

Resolves filesystem paths and checks them against configured `allowed_roots` before privileged filesystem work is forwarded.

### `internal/computer/`

Windows-native screenshots and input control: screen metadata, mouse movement/clicks, wheel scrolling, Unicode typing and key combinations.

### `frontend/`

A local loopback-only interface served by `server.pyw`. It owns:

- first-run and settings pages;
- session/activity/history presentation;
- background-process and connector-error UI;
- local Secret Vault interactions and approval surfaces;
- tunnel setup orchestration and DPAPI-protected runtime-key storage.

The frontend is not the MCP server. It is an operator UI around the local runtime.

### `scripts/`

Windows orchestration:

- `install.ps1` — dependency installation and first build;
- `start-codexpc.ps1` — terminal intro, process cleanup, frontend startup and tunnel supervision;
- `frontend-supervisor.ps1` — keeps the local frontend alive;
- `build.ps1` — format, test, build, smoke and staged deployment;
- restart helpers and the direct stdio wrapper.

## Startup flow

1. `start.cmd` starts `scripts/start-codexpc.ps1`.
2. The wrapper starts the frontend supervisor and waits for a valid setup.
3. The runtime API key is decrypted from DPAPI only when the tunnel process needs it.
4. `tunnel-client` starts the configured profile and launches `dist/codexpc-go.exe` as the stdio MCP command.
5. The Go connector starts one long-lived Codex app-server and exposes its MCP tool surface to ChatGPT.
6. Frontend activity is driven from local structured state/logs; it does not sit in the MCP request path.

## Tool routing

### Filesystem and Codex-native operations

Dedicated filesystem tools use the original Codex filesystem methods after CodexPC validates the path policy. Project rule loading and downstream MCP operations also reuse Codex capabilities where appropriate.

### Terminal

On Windows, general terminal work uses CodexPC's native process-session supervisor. This allows long-running commands to outlive one MCP request and supports polling, stdin and process-tree termination without depending on one app-server request deadline.

Read-only command inspection uses the same native runner with a read-only semantic contract.

### Desktop

Desktop actions are local Windows APIs and never route through a shell.

## State and trust boundary

The default state directory is:

```text
%LOCALAPPDATA%\CodexPCConnector
```

It contains configuration, logs, session state, frontend auth material and DPAPI-protected secrets. `CODEXPC_STATE_DIR` can move the entire state directory.

CodexPC assumes one trusted local Windows account. The frontend binds to `127.0.0.1`; the MCP process is reached through the configured OpenAI tunnel rather than a public listener.

## Safety properties

- allowed-root checks happen before privileged filesystem operations;
- secret material is redacted from normal tool metadata and logs;
- runtime tunnel credentials are encrypted with DPAPI at rest;
- direct secret inspection and sensitive credential injection can require explicit user approval;
- command output is bounded;
- the connector provides emergency cross-session process termination so a stuck command cannot hide behind a dead chat session;
- tunnel changes are validated in an isolated temporary profile before being committed.
