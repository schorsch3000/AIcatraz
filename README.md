# AIcatraz

A jail for AI coding agents on a Linux machine.

The agent gets a Docker container with full freedom inside it. It can read and write the project you mount and nothing else: no home directory, no SSH keys, no other projects. When it needs something that must happen on your machine (a deploy, `git push`, a script that uses a secret), it asks through a narrow door, and **you** decide what that door lets through.

```
 your machine                              container (the jail)
 ┌──────────────────────────┐              ┌────────────────────────────┐
 │ ~/git/project   ─────────┼── rw mount ──┤ ~/git/project              │
 │ ~/git/project/.env       │   hidden     │ .env  (reads as empty)     │
 │ ~/.ssh, ~/other, ...     │   invisible  │ ~     (empty)              │
 │                          │              │                            │
 │  broker  ◄───────────────┼── socket ────┤ `make deploy` ─► shim      │
 │  policy · approval · log │              │ agent: claude / opencode   │
 └──────────────────────────┘              └────────────────────────────┘
```

What it does:

- **Scope containment.** Only the volumes you list exist in the jail, as read-write, read-only, or hidden. A hidden file reads as empty, even though host commands can still use it.
- **A policy for the host.** Commands the agent may run on your machine are listed in the config, with exact arguments. Each is `allow`, `ask` (you get a prompt), `local` (runs inside the jail) or `deny`. Everything else is refused.
- **Pinned scripts.** An allowed `make deploy` really means "run whatever the Makefile says", and the agent can edit the Makefile. So the files a rule depends on are pinned: if one changes, the call asks you again and shows the diff.
- **Coding agents out of the box.** `aicatraz claude` and `aicatraz opencode` install the agent, copy your settings in, start it without its own permission prompts (the jail is the boundary), and remember the session so `aicatraz resume` can continue it.
- **An audit log** of every request, decision and exit.

What it deliberately does **not** do: stop the agent from using the network (it needs its API), or from reading and sending out what you mounted. It contains the scope, not the secrets inside the scope. See [docs/security.md](docs/security.md) for the threat model and the known risks.

## Requirements

- Linux (other systems are out of scope) and Docker (tested with 26.1).
- Go 1.24 or newer, to build.
- `tmux`, if you want `aicatraz up` (approval prompts as a popup). Optional: `--approval-tty` works without it.
- For the agents: a Debian- or Ubuntu-based image. Both agents are dynamically linked against glibc and don't run on Alpine.

## Install

```bash
git clone <this repository> aicatraz && cd aicatraz
make build                                   # creates bin/aicatraz
install -m 0755 bin/aicatraz ~/.local/bin/   # or anywhere on your PATH
aicatraz version
```

`make build` produces a static binary. The same binary is mounted into every jail as the shim, so there is nothing else to install in your images.

Release binaries (`make release`) for linux/amd64 and linux/arm64 come with a `SHA256SUMS` file, and are reproducible: see [Releases](#releases).

## Quick start

In the project you want to put in a jail:

```bash
cd ~/git/myproject
aicatraz init              # writes aicatraz.yaml (use --list to see other starters)
aicatraz config check      # validates it and warns about risky rules
aicatraz run               # an interactive shell in the jail
```

Inside the jail:

```
$ whoami; echo $HOME; ls -A ~
you
/home/you
(nothing: your home exists, and is empty)
$ ls ~/git/myproject
...                        # the project is here, writable
```

Hide a secret, so the agent sees an empty file:

```yaml
volumes:
  - ~/git/myproject:rw
  - ~/git/myproject/.env:hi      # must exist
```

Let the agent deploy through you, with a brake:

```yaml
rules:
  - command: make
    args: [deploy-test]
    decision: allow
    depends_on: auto             # pin the Makefile: a change asks you first
  - command: make
    args: [deploy-prod]
    decision: ask
```

Run a coding agent:

```bash
aicatraz init --example claude --force   # config + Dockerfile.az with node and git
aicatraz claude                          # first run: installs Claude Code, builds the image
aicatraz sessions                        # later: list sessions...
aicatraz resume                          # ...and continue the last one
```

For prompts to appear while the agent works, start it with `aicatraz up --agent claude` inside tmux (they appear as a popup), or give a terminal: `--approval-tty /dev/pts/N`. Without either, `ask` rules are denied, and the denial says why.

## Commands

| Command | What it does |
|---|---|
| `aicatraz init [--example name] [--list] [--force]` | Write a starter `aicatraz.yaml` (and `Dockerfile.az`) into this directory. Never overwrites without `--force`. |
| `aicatraz config check [file]` | Validate a config; warn about rules that are valid but risky. |
| `aicatraz run [flags] [-- command...]` | Start the jail in this terminal. Default command: a shell. |
| `aicatraz up [flags] [-- command...]` | Same, inside a tmux session (`aicatraz-<project>`), so approval prompts pop up. Attaches if it already runs. |
| `aicatraz shell [-- command...]` | Another shell in the running jail (a second terminal). Host commands use the same broker. |
| `aicatraz claude` / `opencode` `[flags] [-- agent args...]` | Start that agent in the jail. Installed on first use. |
| `aicatraz agent` | Start the agent named by `agent.tool` in the config. |
| `aicatraz sessions` | List the agent sessions of this project. |
| `aicatraz resume [id] [--pick]` | Continue the latest (or a given) agent session. |
| `aicatraz tools [list \| install <tool> [version] [--from-host] \| update [tool]]` | Manage the cached agent executables. |
| `aicatraz version` | Print the version. |

Flags for `run`, `up` and the agent commands: `-c file` (config, default `aicatraz.yaml`), `--print` (show the `docker` commands and change nothing), `--approval-tty /dev/pts/N`, `--approve-build` (pin a changed `Dockerfile.az` without asking).

Output is colored on a terminal. `NO_COLOR` turns it off; `AICATRAZ_COLOR=always|never` overrides the detection.

## How the host door works

Inside the jail, the commands named in your rules are shims (symlinks to the `aicatraz` binary, first on `PATH`). When the agent runs `make deploy-test`:

1. the shim sends the call (name, arguments, directory, input) to the **broker**, a process on your machine, over a unix socket;
2. the broker matches it against the rules, first match wins, default deny. Arguments are matched token by token, so `make -f /tmp/evil.mk deploy-test` is not `make deploy-test`;
3. `allow`: it runs on the host, with an environment built from scratch (only what the rule passes on), and its output and exit code come back. `ask`: you are asked first, and shown the exact command, directory, binary and any changed files. `local`: the shim runs the jail's own copy. `deny`: refused;
4. everything is written to `~/.local/state/aicatraz/<project>/audit.log`.

The shim is a convenience, never a boundary: the broker validates and decides everything itself.

## Files and directories

| Where | What |
|---|---|
| `aicatraz.yaml`, `Dockerfile.az` | Your config and the image the jail runs in (in the project). Optional `Dockerfile.az`; without it a plain Debian is used. |
| `~/.local/state/aicatraz/<project>/` | Per-project state: audit log, pins, session index, the agents' config and history. Outside every mount. |
| `~/.local/share/aicatraz/tools/` | Cached agent executables. |
| `$XDG_RUNTIME_DIR/aicatraz/<pid>/` | The broker's socket for a running session. |

A project is identified by the real path of its config file; set `project.id` to pin the id if you move it.

## Documentation

- [docs/configuration.md](docs/configuration.md): every config key.
- [docs/security.md](docs/security.md): threat model, what is enforced, known risks.
- [idea-v2.md](idea-v2.md): the design in full, with the reasoning.
- [milestones.md](milestones.md): how it was built.

## Troubleshooting

| You see | Because | Do |
|---|---|---|
| `volume ...: ... does not exist` | A hidden path must exist at launch (otherwise the agent could create it). | Create the file or directory. |
| `no approval terminal: ask rules will be denied` | Nobody can be asked. | Start with `aicatraz up` inside tmux, or `--approval-tty`. |
| `build files changed since they were last approved` | `Dockerfile.az` is new or changed, and there is no terminal to ask. | Run `aicatraz run` interactively once and answer `p`, or pass `--approve-build`. |
| `the image ... has no glibc dynamic loader` | The agents don't run on Alpine. | Use a Debian/Ubuntu-based image. |
| `hidden path ... is no longer hidden`, session stopped | An editor saved a hidden *file* by renaming a new one over it. | Edit in place, or hide a directory (`.secrets/`). |
| `node: not found` from a hook at agent start | A hook in your own agent settings needs a tool the image lacks. | Install it in `Dockerfile.az`. |
| Docker permission denied | Your user can't reach the Docker socket. | Add yourself to the `docker` group. |

## Development

```bash
make test                # unit tests
make test-integration    # real containers: needs docker and network access
go test -race ./...      # a couple of minutes: the stress tests are heavy
```

Layout: `cmd/aicatraz` (the one binary: launcher, broker, shim and CLI, chosen by the name it is started under), `internal/config` (schema and linter), `internal/jail` (container, volumes, build, tmux), `internal/broker` and `internal/shim` with `internal/proto` (the host door), `internal/policy`, `internal/pins`, `internal/approve`, `internal/canary`, `internal/agent`, `internal/examples`, `internal/ui`.

## Releases

```bash
make release          # dist/aicatraz-<version>-linux-{amd64,arm64} and dist/SHA256SUMS
make release-check    # builds twice, in different directories, and compares the hashes
```

Builds use `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and no build id, so the same source and the same Go toolchain give byte-identical binaries. To verify a release: check out the tag, run `make release` with the Go version shown by `aicatraz version`, and compare `dist/SHA256SUMS`. `make release` refuses to run on a tree with uncommitted changes.
