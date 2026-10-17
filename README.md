# AIcatraz

A jail for AI coding agents on a Linux machine.

Coding agents are useful, fast, and happy to run whatever seems like a good idea at 3 a.m. Giving one your whole home directory, your SSH keys and your network is a plan with a very short post-mortem. AIcatraz puts the agent in a Docker container instead: it can read and write the project you mount and nothing else, it has no network unless you say so, and when it genuinely needs something from your machine (a deploy, a `git push`, a script that uses a secret) it has to ask through a narrow door that **you** control.

```
 your machine                              the jail (a container)
 ┌──────────────────────────┐              ┌────────────────────────────┐
 │ ~/git/project   ─────────┼── rw mount ──┤ ~/git/project              │
 │ ~/git/project/.env       │   hidden     │ .env   (reads as empty)    │
 │ ~/.ssh, ~/other, ...     │   invisible  │ ~      (empty)             │
 │                          │              │                            │
 │  broker  ◄───────────────┼── socket ────┤ `make deploy` ─► shim      │
 │  policy · prompts · log  │              │ the agent                  │
 └──────────────────────────┘              └────────────────────────────┘
```

It contains the *scope*, not the secrets inside it: an agent you let reach the network can still send out what you mounted. [docs/security.md](docs/security.md) says plainly what is and isn't covered.

## What you get

- **Mounts with three modes.** `rw`, `ro`, or `hi` (hidden: the file reads as empty, though host commands can still use it). Your real home is never mounted; the jail's `~` is an empty directory.
- **A door to the host.** The commands the agent may run *on your machine* are listed in the config, with exact arguments. Each is `allow`, `ask` (you get a prompt), `local` (runs inside the jail) or `deny`. Anything not listed is refused.
- **Pinned scripts.** `make deploy` means "do whatever the Makefile says", and the agent can edit the Makefile. So the files a rule depends on are pinned: if one changes, the call asks again and shows the diff.
- **No network by default.** `network: open` for the normal one, or rules that allow named hosts, IPs and ports and ask about the rest. [Details](docs/configuration.md#network).
- **Agents out of the box.** `aicatraz claude` and `aicatraz opencode` install the agent, copy your settings in, and remember sessions for `aicatraz resume`. [Details](docs/agents.md).
- **Sessions you can walk away from.** `aicatraz up` keeps the jail running when you detach (`Ctrl-Y`), shows approval prompts over the agent's screen, and has a status bar.
- **The odds and ends.** Published ports (reach a dev server from your browser), USB devices that survive being unplugged (Arduino, ESP, Pico), `sudo` inside the jail if you must, and an audit log of every request.

## Requirements

Linux, and Docker (tested with 26.1). Go 1.24 to build it. `tmux` only if you want `up --tmux`. The agents need a Debian or Ubuntu based image, because they are built against glibc.

## Install

```bash
git clone <this repository> aicatraz && cd aicatraz
make build                                      # bin/aicatraz, one static binary
install -m 0755 bin/aicatraz ~/.local/bin/      # or anywhere on your PATH
ln -s aicatraz ~/.local/bin/az                  # optional: `az` is a short alias
source <(aicatraz completion bash)              # also: zsh, fish
```

## Five minutes

```bash
cd ~/git/myproject
aicatraz init                # writes aicatraz.yaml; `init --list` shows other starters
aicatraz config check        # checks it and warns about things that look unwise
aicatraz run                 # a shell in the jail
```

You are now in a Debian container as yourself, with the project at the same path as on your machine and nothing else. Try `ls ~`: it is empty, which is the point.

Then add a rule so the agent can do one useful thing on your machine, and see what happens when it asks:

```yaml
# aicatraz.yaml
volumes:
  - .:rw                                  # the directory of this file
  - ./aicatraz.yaml:hi                    # the agent can't read or edit its own rules
  - ./.env:hi                             # a secret it must not see

network: open                             # off by default

rules:
  - command: make
    args: [build, test]
    decision: local                       # runs inside the jail
  - command: make
    args: [deploy]
    decision: ask                         # runs on your machine, after you say yes
    depends_on: auto                      # ...and asks again, with a diff, if the Makefile changed
```

Start an agent in it:

```bash
aicatraz init --example claude --force    # a config and a Dockerfile.az for Claude Code
aicatraz claude                           # first run installs it; later runs just start it
```

## The commands you will use

| | |
|---|---|
| `aicatraz run [-- cmd]` | The jail in this terminal. |
| `aicatraz up` | The jail as a session you can detach from (`Ctrl-Y`) and come back to with `up`. `down` stops it. |
| `aicatraz claude`, `opencode`, `resume` | Agents. In a terminal they run as an `up` session. |
| `aicatraz config check` | Validate the config and lint it. |
| `aicatraz shell` | Another shell in the running jail. |
| `aicatraz devices`, `network`, `pins`, `sessions`, `tools` | Look at, and change, the things their names suggest. |

`aicatraz` with no arguments prints all of them; [docs/commands.md](docs/commands.md) lists every command and flag, where the files live, and what the common error messages mean.

## Things that surprise people

- **"Connection refused" everywhere.** The jail has no network until the config gives it one. Add `network: open`, or rules.
- **A hidden file must exist.** Otherwise the agent could create it, and then it would not be hidden any more.
- **`up` starts below your command and leaves your screen alone.** The bar at the bottom is the status bar; `--status-bar off` removes it, `aicatraz bar-colors` lets you pick a colour.
- **A prompt appeared out of nowhere.** That is the door doing its job. `y` runs it once, `s` for the session, `p` remembers it, anything else says no.
- **The agent says a tool is missing.** The image is yours to fill: `Dockerfile.az` next to the config (`aicatraz init dockerfile` writes a one-line start).

## Where to read more

| | |
|---|---|
| [docs/configuration.md](docs/configuration.md) | Every config key, with defaults and examples. |
| [docs/commands.md](docs/commands.md) | Every command and flag, the files and directories, shell completion, troubleshooting. |
| [docs/sessions.md](docs/sessions.md) | `up`: detaching, the status bar and its colors, the window title, a misbehaving terminal. |
| [docs/security.md](docs/security.md) | The threat model, what is enforced, what is not, and the known risks. |
| [docs/agents.md](docs/agents.md) | Claude Code and opencode: installation, config sync, sessions. |
| [docs/design.md](docs/design.md) | How it works and why: the door, pins, prompts, the audit log. |
| [docs/development.md](docs/development.md) | Building, testing, the code layout, reproducible releases. |
| [milestones.md](milestones.md) | How it was built, and how each part was tested. |

## Development

`make test` runs the unit tests, `make test-integration` starts real containers. Building, the code layout and how releases are made reproducible are in [docs/development.md](docs/development.md).

## License

GPL-3.0-only. See [LICENSE](LICENSE). Every Go file carries the SPDX line, and a test keeps it that way.
