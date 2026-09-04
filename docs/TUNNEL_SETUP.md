# First-run and tunnel setup

CodexPC uses a Windows-first setup flow: install the local runtime once, then configure the workspace and OpenAI tunnel from the local frontend. Manual environment-variable setup is not required for normal use.

## 1. Install

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

CodexPC-managed state and runtimes live under `%LOCALAPPDATA%\CodexPCConnector` by default, so the normal installation does not require an administrator-wide Go setup.

## 2. Local setup page

The installer launches `start.cmd`, which starts the loopback frontend and opens the setup flow.

The main page first checks setup state. When CodexPC is not configured yet it redirects to:

```text
http://127.0.0.1:8765/setup/
```

The setup flow asks for:

1. default workspace;
2. `core` or `full` tool profile;
3. OpenAI Tunnel ID;
4. runtime API key with the required tunnel permissions;
5. optional tunnel profile and organization labels.

## 3. Safe tunnel validation

A new Tunnel ID/key pair is **not** written directly over the working setup.

The frontend server:

1. creates an isolated temporary `tunnel-client` profile;
2. runs `tunnel-client doctor --explain` against the new values;
3. keeps a setup-pending marker so the normal wrapper cannot race the validation;
4. commits the working profile/config only after validation succeeds.

If validation fails, the previous working tunnel profile stays intact and the setup page shows the error.

## 4. Credential storage

The runtime API key is not stored in:

- `config.toml`;
- LocalStorage;
- a batch/PowerShell script;
- the normal tunnel profile metadata.

On Windows it is encrypted with DPAPI for the current Windows account and stored in the CodexPC state directory.

The start wrapper decrypts the value only when starting `tunnel-client`, passes it through the short-lived process environment and clears its own environment copy after the tunnel exits.

## 5. Later launches

Use:

```bat
start.cmd
```

The wrapper:

- shows the short CodexPC terminal intro;
- removes stale copies owned by the previous runtime;
- promotes a staged connector build when one exists;
- starts the frontend supervisor;
- waits for a validated setup when required;
- starts and supervises `tunnel-client`.

Setup can be reopened at any time from the settings button in the CodexPC sidebar. Leaving the runtime-key field blank keeps the currently saved key.

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

### `Waiting for a validated setup`

Open `http://127.0.0.1:8765/setup/` and complete the setup flow. The start wrapper detects a valid saved configuration automatically.

### `tunnel-client` is missing or too old

Run `install.cmd` again. The installer checks the feature set required by the current setup flow and upgrades an incompatible runtime.

### Tunnel validation fails

Check the Tunnel ID, runtime-key permissions and the corresponding OpenAI tunnel configuration. The setup page remains on the current step and does not overwrite the previous working profile.

### Frontend dependency is missing

Run `install.cmd` again. The installer finds supported Python 3.11–3.14 runtimes and stores the exact `pythonw.exe` path for later starts.
