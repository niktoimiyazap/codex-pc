#!/usr/bin/env python3
"""Interactive launcher for CodexPC Connector through OpenAI tunnel-client."""

from __future__ import annotations

import argparse
import getpass
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

import keyring
from keyring.errors import KeyringError

TUNNEL_RE = re.compile(r"^tunnel_[0-9a-f]{32}$")
KEYRING_SERVICE = "CodexPC Connector / OpenAI Tunnel"


def ask(prompt: str, *, default: str | None = None, required: bool = True) -> str:
    suffix = f" [{default}]" if default else ""
    while True:
        value = input(f"{prompt}{suffix}: ").strip()
        if value:
            return value
        if default is not None:
            return default
        if not required:
            return ""
        print("This value is required.")


def locate_tunnel_client() -> str:
    found = shutil.which("tunnel-client")
    if found:
        return found
    raise SystemExit("tunnel-client was not found in PATH. Install it, then reopen the terminal.")


def load_key(profile: str) -> str | None:
    try:
        return keyring.get_password(KEYRING_SERVICE, profile)
    except KeyringError as exc:
        raise SystemExit(f"Unable to read the system credential store: {exc}") from exc


def save_key(profile: str, value: str) -> None:
    try:
        keyring.set_password(KEYRING_SERVICE, profile, value)
    except KeyringError as exc:
        raise SystemExit(f"Unable to save the key in the system credential store: {exc}") from exc


def delete_key(profile: str) -> None:
    try:
        keyring.delete_password(KEYRING_SERVICE, profile)
    except keyring.errors.PasswordDeleteError:
        pass
    except KeyringError as exc:
        raise SystemExit(f"Unable to remove the saved key: {exc}") from exc


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--replace-key", action="store_true", help="replace the saved runtime API key")
    parser.add_argument("--forget-key", action="store_true", help="delete the saved runtime API key and exit")
    parser.add_argument("--profile", default="codexpc", help="local tunnel profile name")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    profile = args.profile
    if args.forget_key:
        delete_key(profile)
        print(f"Saved key removed for profile: {profile}")
        return 0

    print("CodexPC Connector — interactive tunnel launcher")
    print("The API key is stored in Windows Credential Manager or macOS Keychain.\n")
    tunnel_client = locate_tunnel_client()
    organization = ask("Organization name (label only)", required=False)
    profile = ask("Local profile name", default=profile)

    while True:
        tunnel_id = ask("Tunnel ID")
        if TUNNEL_RE.fullmatch(tunnel_id):
            break
        print("Expected: tunnel_ followed by 32 lowercase hexadecimal characters.")

    api_key = None if args.replace_key else load_key(profile)
    if api_key:
        print("Using the runtime API key saved in the system credential store.")
    else:
        api_key = getpass.getpass("Runtime API key (hidden): ").strip()
        if not api_key:
            return 2
        save_key(profile, api_key)
        print("API key saved securely for future launches.")

    python_executable = str(Path(sys.executable).resolve())
    mcp_command = f'"{python_executable}" -m codexpc_connector'
    env = os.environ.copy()
    env["CONTROL_PLANE_API_KEY"] = api_key
    env["CONTROL_PLANE_TUNNEL_ID"] = tunnel_id

    if organization:
        print(f"Organization: {organization}")
    print(f"Profile: {profile}\nTunnel: {tunnel_id}\nAPI key: system credential store")

    init = [
        tunnel_client,
        "init",
        "--sample",
        "sample_mcp_stdio_local",
        "--profile",
        profile,
        "--tunnel-id",
        tunnel_id,
        "--mcp-command",
        mcp_command,
    ]
    result = subprocess.run(init, env=env, check=False)
    if result.returncode:
        return result.returncode
    result = subprocess.run([tunnel_client, "doctor", "--profile", profile, "--explain"], env=env, check=False)
    if result.returncode:
        return result.returncode
    print("\nStarting tunnel. Press Ctrl+C to stop.\n")
    try:
        return subprocess.run([tunnel_client, "run", "--profile", profile], env=env, check=False).returncode
    except KeyboardInterrupt:
        return 130
    finally:
        env.pop("CONTROL_PLANE_API_KEY", None)
        api_key = ""


if __name__ == "__main__":
    raise SystemExit(main())
