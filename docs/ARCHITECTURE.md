# Architecture

CodexPC Connector is a thin local MCP stdio adapter over the original `codex app-server`.

## System boundary

```text
MCP client
    |
    | MCP over stdio
    v
+---------------------------+
| CodexPC Connector         |
|                           |
| server.py                 | MCP lifecycle, logging, cancellation
| tools.py                  | strict schemas and thin adapters
| computer.py               | native Windows desktop control
| mcp_inventory.py          | cached Codex MCP inventory
| security.py               | allowed-root checks and redaction
| app_server.py             | JSON-RPC client for Codex
+-------------+-------------+
              |
              | JSONL / JSON-RPC over stdio
              v
+---------------------------+
| original codex app-server |
|                           |
| fs/readFile               |
| fs/writeFile              |
| fs/readDirectory          |
| fs/createDirectory        |
| fs/copy                   |
| fs/remove                 |
| command/exec              |
| mcpServerStatus/list      |
| mcpServer/tool/call       |
+---------------------------+
```

## Ownership

The original Codex app-server owns:

- filesystem reads and writes;
- directory operations;
- file and directory copying/removal;
- terminal command execution;
- downstream MCP execution.

The connector does not contain its own patch engine, file snapshot cache, project search walker, shell runner, process registry, or background job manager.

The connector still owns:

- MCP tool schemas and dispatch;
- allowed-root validation before forwarding paths;
- bounded and redacted MCP responses and logs;
- persistent MCP inventory caching;
- native Windows screenshot, mouse, keyboard, and scrolling support.

## Request flow

1. The MCP client invokes a connector tool.
2. `server.py` records a bounded, redacted lifecycle event.
3. `tools.py` validates arguments and resolves allowed paths.
4. Filesystem and terminal calls are forwarded to the matching original Codex method.
5. `computer` actions run locally through Windows APIs.
6. The result is normalized and returned to the MCP client.

## Filesystem adapters

| MCP tool | Codex method |
| --- | --- |
| `fs_read_file` | `fs/readFile` |
| `fs_write_file` | `fs/writeFile` |
| `fs_read_directory` | `fs/readDirectory` |
| `fs_create_directory` | `fs/createDirectory` |
| `fs_copy` | `fs/copy` |
| `fs_remove` | `fs/remove` |

The app-server transports file bytes as base64. The connector only performs transport encoding/decoding; it does not read or write the target file itself.

## Terminal adapter

`command_exec` forwards an argv vector to `command/exec`. The connector does not launch the command directly and does not maintain process jobs.

## Desktop control

`computer.py` remains local because desktop screenshots and input are outside the filesystem and terminal APIs. It uses native Windows APIs for:

- screenshots;
- cursor movement;
- mouse clicks;
- wheel scrolling;
- Unicode typing;
- key combinations.

## Security

- All filesystem paths are resolved and checked against configured allowed roots before forwarding.
- Destructive filesystem operations also pass writable-path checks.
- App-server errors and connector logs are redacted.
- The connector is intended for one trusted local user over MCP stdio and must not be exposed directly to a public network.
