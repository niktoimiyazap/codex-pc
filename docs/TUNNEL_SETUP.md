# First-run, Secure MCP Tunnel and ChatGPT setup

CodexPC uses OpenAI Secure MCP Tunnel to let ChatGPT reach the local connector without exposing your PC or MCP server directly to the public internet.

There are three separate pieces to keep straight:

- **Tunnel ID** — identifies the OpenAI-hosted tunnel object.
- **Runtime API key** — authenticates the local `tunnel-client` process. It stays on your PC.
- **ChatGPT custom app** — tells ChatGPT which tunnel to use. It does not need your runtime API key.

Official references:

- Secure MCP Tunnel: https://developers.openai.com/api/docs/guides/secure-mcp-tunnels
- OpenAI tunnel-client: https://github.com/openai/tunnel-client
- ChatGPT developer mode and MCP apps: https://help.openai.com/en/articles/12584461-developer-mode-apps-and-full-mcp-connectors-in-chatgpt-beta

## 1. Install CodexPC

From the repository root:

```bat
install.cmd
```

The installer is idempotent and only installs or repairs what is missing or incompatible:

- the Go version required by `go.mod`;
- Python for `frontend/server.pyw`;
- Codex CLI;
- Codex authentication when needed;
- the official OpenAI `tunnel-client`;
- Go modules and the CodexPC production build.

Downloaded Go and tunnel-client artifacts are SHA-256 verified before use.

CodexPC-managed state and runtimes live under `%LOCALAPPDATA%\CodexPCConnector` by default.

## 2. Get tunnel permissions

Tunnel permissions belong to your OpenAI Platform organization and are separate from ChatGPT Developer mode.

Open:

- Organization roles: https://platform.openai.com/settings/organization/people/roles
- Organization groups: https://platform.openai.com/settings/organization/people/groups

Recommended split:

### Runtime user

Grant:

- **Tunnels: Read**
- **Tunnels: Use**

This is enough for the person/service account whose runtime API key will run `tunnel-client` and for an operator who selects an existing tunnel in ChatGPT.

### Tunnel manager

Grant:

- **Tunnels: Read**
- **Tunnels: Manage**
- **Tunnels: Use** if the same person will also run the tunnel or configure ChatGPT.

`Manage` is needed to create, edit or delete tunnel objects. `Use` is what authorizes the runtime and ChatGPT app to attach to an existing tunnel.

## 3. Create the tunnel and copy the Tunnel ID

Open:

https://platform.openai.com/settings/organization/tunnels

Create a new tunnel for CodexPC.

When creating it, associate the tunnel with:

1. the Platform organization that owns/manages it;
2. the **ChatGPT workspace that will use CodexPC**.

The ChatGPT workspace association matters. A tunnel can exist in Platform and still not appear in ChatGPT if it is only associated with the Platform organization.

After creation, copy the Tunnel ID. It has this shape:

```text
tunnel_0123456789abcdef0123456789abcdef
```

That exact ID must be used by both CodexPC/tunnel-client and the ChatGPT custom app.

## 4. Create the runtime API key

Open:

https://platform.openai.com/settings/organization/api-keys

Create a **Restricted** runtime API key and grant:

- **Tunnels: Read**
- **Tunnels: Use**

The principal that owns the key must also have those tunnel permissions through its Platform role/group.

This key becomes `CONTROL_PLANE_API_KEY` for `tunnel-client doctor` and `tunnel-client run`.

Do **not** use an admin API key as the long-lived runtime key. Admin keys are only needed for CLI tunnel-management commands such as `tunnel-client admin tunnels create|list|update|delete`.

Store the runtime key somewhere safe long enough to paste it into the CodexPC setup UI. CodexPC will encrypt it with Windows DPAPI after validation.

## 5. Finish setup in the CodexPC local UI

The installer launches `start.cmd`, which starts the loopback frontend and opens the setup flow.

If needed, open it manually:

```text
http://127.0.0.1:8765/setup/
```

Enter:

1. default workspace;
2. `core` or `full` tool profile;
3. the OpenAI Tunnel ID;
4. the Restricted runtime API key;
5. optional tunnel profile and organization labels.

A new Tunnel ID/key pair is **not** written directly over a working setup.

The frontend server:

1. creates an isolated temporary `tunnel-client` profile;
2. runs `tunnel-client doctor --explain` against the new values;
3. keeps a setup-pending marker so the normal wrapper cannot race validation;
4. commits the working profile/config only after validation succeeds.

If validation fails, the previous working tunnel profile stays intact and the setup page shows the error.

## 6. Connect CodexPC in ChatGPT

Keep `start.cmd` running while you create or scan the app. Tool discovery depends on a healthy local tunnel runtime.

Open ChatGPT on the web and enable Developer mode if your account/workspace requires it.

Current OpenAI setup paths are:

- user settings: **Settings → Apps → Advanced Settings** to enable Developer mode where available;
- create an app: **Settings → Apps → Create**;
- workspace admins/owners can also use **Workspace settings → Apps → Create**.

Then create the CodexPC app:

1. click **Create**;
2. choose **Tunnel** under **Connection**;
3. select the tunnel from the list, or paste the `tunnel_...` ID;
4. choose the app authentication mechanism if your deployment adds one — the CodexPC tunnel runtime key itself is not entered here;
5. click **Scan Tools**;
6. wait for CodexPC tools such as `session_create`, `fs_read_file`, `command_exec` and `multi_tool` to appear;
7. click **Create**.

The app should then appear under enabled apps with a developer/custom label. In a chat, select CodexPC for the message where you want ChatGPT to use it.

OpenAI currently documents full custom MCP write/modify support for ChatGPT Business and Enterprise/Edu. Pro users can use custom MCP apps in Developer mode with read/fetch permissions. MCP support is still evolving, so exact UI labels may move.

## 7. Normal daily startup

After first setup, use:

```bat
start.cmd
```

The wrapper:

- shows the short CodexPC terminal intro;
- starts the local frontend supervisor;
- restores the validated tunnel configuration;
- decrypts the runtime key only when starting `tunnel-client`;
- keeps the tunnel supervised.

You do not need to recreate the ChatGPT app or generate a new key on every launch.

## Credential storage

The runtime API key is not stored in:

- `config.toml`;
- LocalStorage;
- a batch/PowerShell script;
- the normal tunnel profile metadata.

On Windows it is encrypted with DPAPI for the current Windows account and stored in the CodexPC state directory.

The start wrapper decrypts the value only when starting `tunnel-client`, passes it through the short-lived process environment and clears its own environment copy after the tunnel exits.

## State files

Default state directory:

```text
%LOCALAPPDATA%\CodexPCConnector
```

Important setup/runtime files include:

```text
config.toml                  non-secret CodexPC configuration
tunnel-runtime-key.dpapi     DPAPI-protected runtime key
setup.pending.json           transient validation marker
wrapper.pid                  active start-wrapper ownership
logs\                        structured runtime logs
```

Set `CODEXPC_STATE_DIR` before startup to move the whole state directory.

## Troubleshooting

### The tunnel exists in Platform but does not appear in ChatGPT

Check all three:

1. the tunnel is associated with the target ChatGPT workspace, not only with a Platform organization;
2. the ChatGPT app creator has **Tunnels: Read + Use**;
3. CodexPC/tunnel-client is running and ready.

You can still paste the `tunnel_...` ID manually when ChatGPT offers that option.

### `Waiting for a validated setup`

Open `http://127.0.0.1:8765/setup/` and complete the setup flow. The start wrapper detects a valid saved configuration automatically.

### `tunnel-client` is missing or too old

Run `install.cmd` again. The installer checks the feature set required by the current setup flow and upgrades an incompatible runtime.

### Tunnel validation returns 401 or 403

Confirm that:

- the runtime key is the Restricted key you intended to use;
- its owner has **Tunnels: Read + Use**;
- the key itself has **Tunnels: Read + Use**;
- the Tunnel ID belongs to the expected Platform organization/workspace context.

Do not replace the runtime key with an admin key just to bypass the error.

### `Scan Tools` cannot reach CodexPC

Keep `start.cmd` running and verify that the local tunnel runtime is healthy. A tunnel object can remain visible in Platform even while the local `tunnel-client` process is stopped.

### Frontend dependency is missing

Run `install.cmd` again. The installer finds supported Python 3.11–3.14 runtimes and stores the exact `pythonw.exe` path for later starts.
