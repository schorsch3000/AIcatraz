# Changelog

## 0.3.1 (2026-10-20)

- Fix: the status bar of `aicatraz up` stayed damaged when the program never went quiet, such as opencode's thinking spinner drawing over it. The bar is now repainted at least once a second even while the program is writing, and absolute row addressing (CUP, HVP, VPA) is kept above the bar row.

## 0.3.0 (2026-10-19)

- When there is no user config, the first `init`, `run`, `up`, `claude`, ... writes `~/.config/aicatraz/config.yaml` with every setting commented out at its default. An existing file is never changed.
- `init.dockerfile.lines` in the user config replaces the upgrade line that `aicatraz init dockerfile` writes; `""` writes none.

## 0.2.0 (2026-10-19)

- Approval prompts in `aicatraz up` no longer take over the screen while you type. A prompt that arrives while you are typing waits: the status bar blinks `⚠️ Network host:port  Ctrl-Q` (or `Command ...`), and the prompt opens after `approval.idle` (5s) without typing, or when you press Ctrl-Q. A prompt that opens still ignores keys for `approval.guard` (500ms), and its timeout now starts when it opens. `bar.symbol` and `bar.symbol_width` set the symbol in front of the message and the columns it takes.
- New user config, `~/.config/aicatraz/config.yaml`, for `approval`, `canary`, `global`, `agent`, `limits`, `env`, `network` and `bar`. The project's file overrides it key by key; volumes, image, rules, ports, devices and sudo stay project-only.
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
