# Changelog

## 0.7.0 (2026-10-20)

- The image build uses `Dockerfile.az.dockerignore`, the ignore file docker reads for `Dockerfile.az`. The hidden-path check, the build-file pinning and the offer to add missing paths all use it; a plain `.dockerignore` is no longer checked.
- `aicatraz init` writes an empty `Dockerfile.az.dockerignore` (an existing one is kept).
- A project with a `Dockerfile.az` and a `.dockerignore` but no `Dockerfile.az.dockerignore` is asked, in a terminal, whether to rename the `.dockerignore` or create a blank `Dockerfile.az.dockerignore`. This covers `run`, `up` and the agent commands. Without a terminal nothing is asked or changed.

## 0.6.1 (2026-10-20)

- With the `rainbow` theme a waiting prompt no longer blinks the bar's message; the rainbow turns (once around in 4 seconds) until the prompt opens.

## 0.6.0 (2026-10-20)

- `aicatraz run` in a terminal now runs as an `up` session, like the agent commands: status bar, detach, and approval prompts over the screen. `--no-session` runs it directly as before; without a terminal, or with `--approval-tty`, it always does.

## 0.5.1 (2026-10-20)

- Fix (possibly): Shift+Enter could submit the prompt instead of adding a line after a popup. The session now follows keyboard flags that a program sets (`CSI = flags u`) as well as pushes, and restores them when a popup closes.

## 0.5.0 (2026-10-20)

- New bar themes: `lime`, `coral`, `rose`, `ocean` and `rainbow`. `rainbow` is a gradient from red to violet across the bar (black text); a background of your own (`--bar-bg`, `bg:`) turns the gradient off. `aicatraz bar-colors` shows all 23 themes.

## 0.4.0 (2026-10-20)

- Fix: since 0.3.1 the hidden-path check and the device node maker did nothing. The change that makes the CLI refuse to run inside a jail also refused the two internal commands the launcher runs inside it, so a hidden file that became visible was not noticed, and device nodes were not made. Those two commands are allowed again.
- CI now runs the integration tests (real containers). Two of them were out of date and are fixed.
- Command line: every flag is checked against one table per command. A mistake now names the flag and the command (`unknown flag --x for aicatraz up`) and points to `aicatraz help up`, instead of printing the whole usage. `aicatraz help [command]` and `<command> --help` list the flags of a command and never read a config. `--flag=value` works for long flags, `-c file` works the same on every command (including `config check`), and unknown commands suggest the closest name.
- The usage text is shorter and grouped; flags that it left out (`--agent`, `--resume`, `--approval-tty`, `--approval-sock`, `--no-session`) are in `help <command>`. Exit codes are documented.
- A missing `aicatraz.yaml` now says to run `aicatraz init` or use `-c`, instead of `open aicatraz.yaml: no such file or directory`.
- New: `aicatraz status` (running jails of all projects), `log [-f] [-n N]` (the audit log, readable), `doctor` (checks Docker and the rest) and `clean` (removes state and images of projects whose config is gone; asks first).
- The user config is now offered, not written silently: it asks first, only in a terminal, and remembers a no.
- Docs: install from a release binary; which of `claude`, `up` and `run` to use. `milestones.md` is removed.

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
