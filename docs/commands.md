# Commands, files and troubleshooting

The README has the short version. This is the full list.

## The commands

| Command | What it does |
|---|---|
| `aicatraz init [--example name] [--list] [--force]` | Write a starter `aicatraz.yaml` and a `Dockerfile.az` (and an empty `Dockerfile.az.dockerignore`) into the `.az/` directory of this project, plus the files of the user config's `copy` section (those go into the project directory itself). The `Dockerfile.az` is the example's own if it has one (`claude`, `opencode`); otherwise it is `FROM debian:13`, `FROM ubuntu:24.04`, ... matching the distribution this machine runs (`debian:stable-slim` if that is unknown), followed by a `RUN` line that upgrades the packages, with the install command commented out for you to extend (`apt`, `dnf`, `yum`, `apk`, `pacman` or `zypper`, as the distribution uses). Knows Debian, Ubuntu, Fedora, Alpine, Rocky, AlmaLinux, CentOS Stream, RHEL, Arch, openSUSE, Amazon Linux and their Ubuntu and Debian derivatives. The starter sets `project.id` from the directory name, asks about every network connection (`network: {default: ask}`) and picks a random status bar theme. Never overwrites without `--force`. |
| `aicatraz completion bash\|zsh\|fish` | Print shell completion code for `aicatraz` and `az`; see [Shell completion](#shell-completion). |
| `aicatraz config check [file]` | Validate a config; warn about rules that are valid but risky. |
| `aicatraz run [flags] [-- command...]` | Start the jail in this terminal. Default command: a shell. In a terminal it runs as an `up` session (status bar, detach, approval prompts over the screen; same flags); `--no-session` runs it directly. Without a terminal it always runs directly. |
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
| `aicatraz status` | The running jails of all projects (not the network helpers). |
| `aicatraz log [-f] [-n N]` | The audit log of this project, newest last: host commands asked for and what was decided, network prompts, devices, with the pin warnings. Shows the last 20 entries (`-n 0`: all); `-f` keeps following. The raw JSON lines are in `~/.local/state/aicatraz/<project>/audit.log`. |
| `aicatraz doctor` | Check Docker (installed, daemon reachable, permissions), `tmux` (optional), the runtime and state directories, the user config and the project's config. Exits 1 if something that stops AIcatraz is wrong. |
| `aicatraz clean [--yes]` | Remove the state directory and the images of projects whose config file no longer exists. It lists them and asks first; without a terminal it needs `--yes`. A project moved without `project.id` set looks gone here, so run the project once from its new place first. Projects started with an older version have no recorded location and are never touched. |
| `aicatraz help [command]` | The usage, or the flags of one command. `aicatraz <command> --help` does the same. |
| `aicatraz version` | Print the version. |

Flags for `run`, `up` and the agent commands: `-c file` (config, default `.az/aicatraz.yaml`, or an `aicatraz.yaml` in the current directory from before `.az`), `--print` (show the `docker` commands and change nothing), `--approval-tty /dev/pts/N`, `--approve-build` (pin a changed `Dockerfile.az` without asking). `aicatraz help <command>` lists the flags of each command.

**One way to write flags, everywhere.** Every command that reads a project's config takes `-c file` (or `--config file`), also `config check -c file`, and anywhere on the line. Long flags also take `--flag=value`. A flag a command does not have is an error that names the flag and the command; nothing is guessed. After `--` everything belongs to the program in the jail.

**Missing config.** Commands that need `.az/aicatraz.yaml` say so, and what to do (`aicatraz init`, or `-c file`), instead of failing on the file name.

**Status bar flags.** `--status-bar bottom|off` turns the bar on or off. Its colors are `--bar-theme <name>` (a theme from `bar-colors`, or `random`) and `--bar-fg` / `--bar-bg` (a color); to keep a choice, use the `bar` section of the user or project config instead (see [sessions.md](sessions.md#the-status-bar)).

**Exit codes.** `0` ok. `1` a config with errors (`config check`, `doctor`), or `init` would overwrite. `2` anything else that went wrong, including a bad command line. `run`, `up` and the agent commands exit with the code of the program that ran in the jail.

Output is colored on a terminal. `NO_COLOR` turns it off; `AICATRAZ_COLOR=always|never` overrides the detection.

## Files and directories

| Where | What |
|---|---|
| `.az/aicatraz.yaml`, `.az/Dockerfile.az` | Your config and the image the jail runs in (in the project). Optional `Dockerfile.az`; without it a plain Debian is used. |
| `~/.local/state/aicatraz/<project>/` | Per-project state: audit log (`aicatraz log`), pins, session index, the agents' config and history, and `project` (the path of the config, for `clean`). Outside every mount. |
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
| The agent can't reach its service, `connection refused` everywhere | The jail has no network unless the config gives it one (the starter from `init` asks about each connection instead). | Add `network: open`, or rules (see [`network`](configuration.md#network)). |
| `no .az/aicatraz.yaml in this directory` | You are not in a project, or it has no config yet. | `cd` to the project, run `aicatraz init`, or name a file with `-c`. |
| `unknown flag --x for aicatraz up` | The command has no such flag. | `aicatraz help up` lists the flags it has. |
| `aicatraz: refusing to run inside a jail` | The CLI was started inside a jail (the broker socket is mounted). | Run it on the host. `az` inside a jail stays a proxied command. |
| `no approval terminal: ask rules will be denied` | Nobody can be asked. | Start with `aicatraz up`, or pass `--approval-tty`. |
| `build files changed since they were last approved` | `Dockerfile.az` is new or changed, and there is no terminal to ask. | Run `aicatraz run` interactively once and answer `p`, or pass `--approve-build`. |
| `the image ... has no glibc dynamic loader` | The agents don't run on Alpine. | Use a Debian/Ubuntu-based image. |
| `hidden path ... is no longer hidden`, session stopped | An editor saved a hidden *file* by renaming a new one over it. | Edit in place, or hide a directory (`.secrets/`). |
| `node: not found` from a hook at agent start | A hook in your own agent settings needs a tool the image lacks. | Install it in `Dockerfile.az`. |
| Docker permission denied | Your user can't reach the Docker socket. | Add yourself to the `docker` group. |
