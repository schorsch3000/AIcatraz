# Changelog

## Unreleased

- The starter `aicatraz.yaml` written by `aicatraz init` now sets `project.id` (from the directory name), uses `network: {default: ask}` so every connection is asked about, and has an active `bar:` section with a theme drawn at random.
- `aicatraz init dockerfile` adds a `RUN` line that upgrades the image's packages (`apt`, `dnf`, `yum`, `apk`, `pacman` or `zypper`, to match the distribution), with the install command commented out to extend.

## 0.1.2 (2026-10-18)

- Fix: `aicatraz up`, `claude` and `agent` failed with "the session server did not start" when the binary was reached through a symlink to a release file such as `aicatraz-v0.1.1-linux-amd64`. The re-executed server saw its resolved file name and ran as the shim. Any `aicatraz-*` name now runs the CLI.

## 0.1.0 (2026-10-18)

First release.

- Jail: hardened `docker run` from a YAML config, with `rw`, `ro` and `hi` mounts, an empty home, dropped capabilities, read-only root and docker-volume scratch space.
- Door to the host: broker and shim with `allow`, `ask`, `local` and `deny` rules, environment whitelist, pinned dependent files, audit log.
- Network: off by default, optional allowlist with prompts.
- Agents: `aicatraz claude` and `aicatraz opencode`, with session resume.
- Sessions: built-in multiplexer (`aicatraz up`, `Ctrl-Y` to detach, status bar, info popup) or tmux (`up --tmux`).
- Published ports, USB devices, canary check for hidden files, config linter.
- Static, reproducible linux/amd64 and linux/arm64 binaries (`make release`).
