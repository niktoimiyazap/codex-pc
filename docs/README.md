# CodexPC documentation

The root [README](../README.md) is the product overview. This directory contains the implementation and operations details.

| Document | What it covers |
| --- | --- |
| [Architecture](ARCHITECTURE.md) | Runtime components, trust boundaries and request flow |
| [Configuration](CONFIGURATION.md) | Supported TOML keys, state directory and launcher overrides |
| [First-run setup](TUNNEL_SETUP.md) | Windows installer, local setup UI, tunnel validation and credentials |
| [Security policy](../SECURITY.md) | Security assumptions and vulnerability reporting |
| [Contributing](../CONTRIBUTING.md) | Development workflow and contribution expectations |
| [Changelog](../CHANGELOG.md) | Notable project changes |

## User path

For a normal Windows installation you should not need to edit these files by hand:

```text
install.cmd  →  local /setup/ page  →  start.cmd
```

Manual configuration and developer scripts are documented for source builds, debugging and advanced setups.
