# Commands, files and troubleshooting

The README has the short version. This is the full list.

## The commands

| Command | What it does |
|---|---|
| `aicatraz init [--example name] [--list] [--force]` | Write a starter `aicatraz.yaml` (and `Dockerfile.az`) into this directory. Never overwrites without `--force`. |
| `aicatraz init dockerfile [--force]` | Write a `Dockerfile.az` (`FROM debian:13`, `FROM ubuntu:24.04`, ...) matching the distribution this machine runs, followed by a `RUN` line that upgrades the packages, with the install command commented out for you to extend (`apt`, `dnf`, `yum`, `apk`, `pacman` or `zypper`, as the distribution uses). Knows Debian, Ubuntu, Fedora, Alpine, Rocky, AlmaLinux, CentOS Stream, RHEL, Arch, openSUSE, Amazon Linux and their Ubuntu and Debian derivatives. Never overwrites without `--force`. |
| `aicatraz completion bash\|zsh\|fish` | Print shell completion code for `aicatraz` and `az`; see [Shell completion](#shell-completion). |
| `aicatraz config check [file]` | Validate a config; warn about rules that are valid but risky. |
| `aicatraz run [flags] [-- command...]` | Start the jail in this terminal. Default command: a shell. |
| `aicatraz up [flags] [-- command...]` | Same, as a session that keeps running when you detach (`Ctrl-Y`, or `--detach-key ctrl-<key>` / `$AICATRAZ_DETACH_KEY`), and that shows approval prompts over the agent's screen. Attaches if it already runs. A status bar on the bottom row shows the project, what runs, how long, and the detach key (`Ctrl-Q` shows the session's settings: `--info-key`, `$AICATRAZ_INFO_KEY`; `--status-bar off`, colors: `--bar-theme`, `--bar-fg`, `--bar-bg`). `--tmux` uses a tmux session instead. |
| `aicatraz down` | Stop the project's session (the jail shuts down). |
| `aicatraz devices [-c file]` | List the USB devices of this machine (vendor:product, serial number, device nodes) and which of them the config hands to the jail. |
| `aicatraz network [-c file] list` | Show the network rules of the config and the ones remembered from prompts. |
| `aicatraz network [-c file] allow\|deny <target> [--ports ...]` · `forget <number> \| --all` | Change the remembered network rules (also in a running session). |
| `aicatraz pins [-c file] list` | Show the files whose approved version is remembered, and whether each is still as approved (`ok`, `changed`, `missing`). |
| `aicatraz pins [-c file] diff [<path>...]` | Show what changed in pinned files since they were approved (all changed ones, or the files you name). |
| `aicatraz pins [-c file] forget <path>... \| --missing \| --all` | Forget pins: the next time a rule runs them they need approval again. `--missing` drops pins of files that are gone. |
| `aicatraz bar-colors` | Show the status bar in every color theme. |
| `aicatraz shell [-- command...]` | Another shell in the running jail (a second terminal). Host commands use the same broker. |
| `aicatraz claude` / `opencode` `[flags] [-- agent args...]` | Start that agent in the jail. Installed on first use. In a terminal it runs as an `up` session (same flags; `--no-session` to run it directly). |
| `aicatraz agent` | Start the agent named by `agent.tool` in the config. |
| `aicatraz sessions` | List the agent sessions of this project. |
| `aicatraz resume [id] [--pick]` | Continue the latest (or a given) agent session. |
| `aicatraz tools [list \| install <tool> [version] [--from-host] \| update [tool]]` | Manage the cached agent executables. |
| `aicatraz version` | Print the version. |

Flags for `run`, `up` and the agent commands: `-c file` (config, default `aicatraz.yaml`), `--print` (show the `docker` commands and change nothing), `--approval-tty /dev/pts/N`, `--approve-build` (pin a changed `Dockerfile.az` without asking).

Output is colored on a terminal. `NO_COLOR` turns it off; `AICATRAZ_COLOR=always|never` overrides the detection.

## Files and directories

| Where | What |
|---|---|
| `aicatraz.yaml`, `Dockerfile.az` | Your config and the image the jail runs in (in the project). Optional `Dockerfile.az`; without it a plain Debian is used. |
| `~/.local/state/aicatraz/<project>/` | Per-project state: audit log, pins, session index, the agents' config and history. Outside every mount. |
| `~/.local/share/aicatraz/tools/` | Cached agent executables. |
| `$XDG_RUNTIME_DIR/aicatraz/<pid>/` | The broker's socket for a running session. |

A project is identified by the real path of its config file; set `project.id` to pin the id if you move it.

## Shell completion

`aicatraz completion <shell>` prints code to source. It completes commands, flags, example names, agent names and files, for both `aicatraz` and `az`.

```bash
source <(aicatraz completion bash)       # bash (add to ~/.bashrc); `sh` prints the same bash code
source <(aicatraz completion zsh)        # zsh (add to ~/.zshrc, after compinit)
aicatraz completion fish | source        # fish (or save to ~/.config/fish/completions/aicatraz.fish)
```

## Troubleshooting

| You see | Because | Do |
|---|---|---|
| `the file is not read from here on: this line is indented differently ...` | A line is indented less than the first key of the file, and YAML stops reading there: usually an example switched on by removing only the `#`, which leaves a space in front of the key. | Remove the space too, so every top-level key starts in column 1. |
| `volume ...: ... does not exist` | A hidden path must exist at launch (otherwise the agent could create it). | Create the file or directory. |
| The agent can't reach its service, `connection refused` everywhere | The jail has no network unless the config gives it one. | Add `network: open`, or rules (see [`network`](configuration.md#network)). |
| `no approval terminal: ask rules will be denied` | Nobody can be asked. | Start with `aicatraz up`, or pass `--approval-tty`. |
| `build files changed since they were last approved` | `Dockerfile.az` is new or changed, and there is no terminal to ask. | Run `aicatraz run` interactively once and answer `p`, or pass `--approve-build`. |
| `the image ... has no glibc dynamic loader` | The agents don't run on Alpine. | Use a Debian/Ubuntu-based image. |
| `hidden path ... is no longer hidden`, session stopped | An editor saved a hidden *file* by renaming a new one over it. | Edit in place, or hide a directory (`.secrets/`). |
| `node: not found` from a hook at agent start | A hook in your own agent settings needs a tool the image lacks. | Install it in `Dockerfile.az`. |
| Docker permission denied | Your user can't reach the Docker socket. | Add yourself to the `docker` group. |
