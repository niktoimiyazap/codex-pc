# Python legacy archive

This directory contains the former Python implementation of CodexPC Connector as it existed immediately before the Go migration became the active runtime.

It is intentionally archived, not deleted, because the Python tree contained local uncommitted work that may still be useful for reference or regression comparison.

Archived here:
- `codexpc_connector/`
- Python tests
- Python packaging (`pyproject.toml`)
- legacy entrypoints/wrappers
- Python-only smoke/benchmark/setup helpers

The active connector runtime lives under `cmd/` and `internal/` and is built with `scripts/build.cmd`.

Do not add new production changes to this archived implementation. If a historical behavior is needed, port it to Go instead.
