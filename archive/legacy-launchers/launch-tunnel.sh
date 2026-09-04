#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

if command -v python3 >/dev/null 2>&1; then
  exec python3 scripts/setup_tunnel.py
fi

printf '%s\n' 'Error: Python 3 was not found in PATH.' >&2
exit 2
