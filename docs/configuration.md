# Configuration reference

The config is one YAML file, `.az/aicatraz.yaml` by default. Relative paths in it (`./`, `../`) are relative to the project directory, the one holding `.az/`; for a config anywhere else, to the file's own directory. Unknown keys are errors, and `aicatraz config check` reports every problem with its line number. Everything is optional; an empty file is a valid config (an empty jail with nothing mounted).

```yaml
project:    { id: ... }
image:      ...
volumes:    [ ... ]
env:        { static: {...}, from_host: [...] }
sudo:       false
notifications: { shim: true, osc: true }
bar:        { theme: forest, fg: white, bg: "#102030", symbol: "⚠️", symbol_width: 2 }
ports:      [ 3000, "8080:80" ]
devices:    [ {serial: "usb-Arduino_*"}, {usb: "2e8a:*"}, {path: /dev/ttyUSB0} ]
network:    off | open | { default: ask, dns: open, resolvers: [...], rules: [ ... ] }
limits:     { pids: 1024, memory: 2g }
approval:   { timeout: 2m, idle: 5s, guard: 500ms }
canary:     { action: stop, interval: 3s }
global:     { env_strip: [...] }
rules:      [ ... ]
agent:      { tool: claude, version: latest, skip_permissions: true }
```

## The user config

Settings that are about you, not the project, can go into `~/.config/aicatraz/config.yaml` (or `$XDG_CONFIG_HOME/aicatraz/config.yaml`). The first time you run `init`, `run`, `up`, `claude`, `opencode`, `agent`, `shell`, `down` or `resume` (not with `--print`) from a terminal and there is no such file, AIcatraz asks whether to write one with every setting commented out at its default (Enter says yes). It never writes it without asking, never when nobody can answer (a script, a pipe), and never touches an existing file. If you say no it remembers that (`~/.config/aicatraz/.no-user-config`; remove that file to be asked again). It is read from the host and never mounted into a jail. It has the same format as the project's file, and may hold only `approval`, `canary`, `global`, `agent`, `limits`, `env`, `network`, `bar`, `init`, `notifications`, `copy` and `setup`; anything else (volumes, image, rules, ports, devices, `sudo`, `project`) is an error, so a setting you forgot about can't add mounts, host commands, ports, devices or root to a jail. `network` can still widen one: `network: open` there applies to every project without its own `network` section.

```yaml
# ~/.config/aicatraz/config.yaml
approval: { idle: 8s }
notifications: { shim: true }
bar: { theme: random, symbol: "⚠️", symbol_width: 2 }
network: { default: ask }
```

`init.dockerfile.lines` replaces the package upgrade line that `aicatraz init` writes after `FROM` (only the user config is read for this). Without it the line for your distribution is written; `""` writes none.

```yaml
init:
  dockerfile:
    lines: |
      RUN apt update && apt upgrade -y && apt install -y git curl
```

`copy` puts files you always want into the project directory (the one holding `.az/`) when you run `aicatraz init` (not on later starts). A target that already exists is never overwritten, and nothing is copied over a symlink or through a directory that leads out of the project. A file that can't be copied is reported and skipped. Only the user config reads `copy`.

```yaml
copy:
  - {from: ~/dotfiles/editorconfig, to: .editorconfig}   # `from`: absolute, ~/..., or relative to the user config's directory
  - {from: prompts/CLAUDE.md, to: CLAUDE.md}             # `to`: relative, inside the project
  - {from: ~/dotfiles/lint.cfg, to: tools/}              # `to` ending in `/`: a directory, the file keeps its name (tools/lint.cfg)
```

A key the project's file sets wins over the user's, key by key (`approval.timeout` in the project, `approval.idle` from you). `network` is the exception: if the project has a `network` section (even `network: off`), it replaces yours as a whole. Problems in the user config are reported when a project config is loaded, with the file's path.

## `project`

| Key | Default | |
|---|---|---|
| `id` | derived | Name of the project's state directory and image. By default: the config directory's name plus a short hash of the config file's real path. Set it to keep history and pins when you move the project. Letters, digits, `.`, `_`, `-`. |

## `image`

What the container is made from.

```yaml
image: debian:13                 # a plain image reference, no build
image:                           # or a build
  dockerfile: .az/Dockerfile.az   # default if the file exists
  context: .                     # build context, default: the config's directory
```

With neither, `.az/Dockerfile.az` (or a `Dockerfile.az` in the project directory, from before `.az`) is built if it exists; otherwise `debian:stable-slim` is used. The image is built on every start (Docker's cache makes that quick) and tagged per project.

The build files (`Dockerfile.az`, the ignore files, and single files named by `COPY`/`ADD`) are pinned: a new or changed one needs your approval before the build. Every hidden path inside the build context must be excluded by `Dockerfile.az.dockerignore` (next to `Dockerfile.az`, in `.az/`; `init` writes an empty one), otherwise the build is refused, since `COPY . /app` would put it in the image. In a terminal, `run` and `up` offer to add each missing path to the ignore file (creating `Dockerfile.az.dockerignore` if there is none): one question per file and path, every time, and only a yes changes anything. Without a terminal, or with `--print`, nothing is asked or written and the build is refused. If a project has a `.dockerignore` but no `Dockerfile.az.dockerignore`, `run` and `up` ask in a terminal whether to rename it or create a blank `Dockerfile.az.dockerignore`. Docker (BuildKit) reads `Dockerfile.az.dockerignore` instead of `.dockerignore` for a build with `Dockerfile.az`.

The image's `ENTRYPOINT` and `CMD` are replaced. The container runs as your UID and GID whatever the image's `USER` says.

## `setup`

```yaml
setup:
  - gb-ensure-bender
  - git config --global pull.rebase true
```

Shell commands run inside the jail, in order, right before the agent or shell starts. They run as your UID and GID with the jail's mounts, environment, `--read-only` root and empty capability set, so a hook can prepare the home directory, per-repo state and generated config but cannot do anything root-only (bake that into `Dockerfile.az`). Each entry is a line of `sh`; the script runs with `set -e`, so a failing command stops the launch before the agent starts. The agent is then `exec`'d and keeps the terminal and signals. A script you call must be in the image or in a read-only volume: one the agent can write is one the agent controls. It may be set in the user config (applies to every project without its own `setup`) or in the project file, which replaces it.

## `volumes`

```yaml
volumes:
  - .:rw                 # the directory holding this file
  - ~/tools/mytool:ro
  - ./.env:hi
```

`path:mode`. A path may be absolute, start with `~/` (your home), or start with `./` or `../`: those are relative to the directory of the config file, not to where you run the command. A config with only relative paths works for every teammate, wherever they check the project out. (A bare `foo` is an error, to catch typos.) Whatever the form, the path is resolved on the host and is the **same inside the container**.

| Mode | In the jail |
|---|---|
| `rw` | read and write |
| `ro` | read only |
| `hi` | hidden: a file reads as empty (`/dev/null` is mounted over it; it can't be deleted or renamed, writes are discarded), a directory is an empty read-only tmpfs |

**Nesting: the deepest entry wins, and the order in the file does not matter.** Entries are mounted parents first, so a path gets the mode of the most specific entry that contains it. These two lists mean the same:

```yaml
volumes:                        volumes:
  - ~/p/sub/deep:hi               - ~/p:rw
  - ~/p/sub:ro                    - ~/p/sub:ro
  - ~/p:rw                        - ~/p/sub/deep:hi
```

This differs on purpose from [rules](#rules), where the first match wins: paths form a tree, so "more specific" is always well defined, while rules overlap in ways that have no natural order. A deeper entry may loosen as well as tighten: `rw` inside `ro` makes that part writable, and an `rw` or `ro` entry inside a hidden directory shows that one path again (hide a directory except one file). Paths are compared as written (after `~` and `./` are resolved), symlinks are not followed, so an entry that reaches a volume through a symlink does not count as nested in it.

Rules: every path must exist at launch; `hi` entries must lie inside an `rw` or `ro` entry; no duplicates; no commas in paths; you can't mount `/`, your home directory or a parent of it. Your home path exists in the jail but is an empty directory, so `~` resolves and there is nothing to find.

## `env`

```yaml
env:
  static:  { EDITOR: vim }       # set in the jail
  from_host: [LANG]              # copied from your environment at launch
```

`from_host` passes names only, so values never show up in a process list. Secret-looking names (`*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`) get a warning: the agent can read them and has network access. A `PATH` in `static` is respected; the shim directory is always put in front.

## `sudo`

```yaml
sudo: true      # default: false
```

Gives the jail user passwordless `sudo`. **This loosens the jail, read [the risks](security.md#sudo) first.** With `sudo: true`:

1. **The user may use sudo without a password.** A drop-in file `/etc/sudoers.d/aicatraz` names your host user by number (`#1000 ALL=(ALL:ALL) NOPASSWD: ALL`), and PAM is set to accept the user, since the jail's passwd file is generated and has no password entry.
2. **sudo is installed if the image doesn't have it.** Before the first start, aicatraz builds a small layer on top of your image (tagged `aicatraz/<project>:sudo`) that installs sudo and adds the sudoers entry. It knows how to install sudo on Debian, Ubuntu, Fedora, Alpine, Rocky, AlmaLinux, CentOS Stream, RHEL, Arch, openSUSE, Amazon Linux and their derivatives. For any other image it stops with an error: install sudo in your `Dockerfile.az` yourself, and aicatraz still adds the sudoers entry. An image without a shell can't be used.

To make sudo work, the container runs differently: `no-new-privileges` is off, the root filesystem is writable (changes are thrown away when the container exits), and ten capabilities are kept instead of none (`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `SETUID`, `SETGID`, `SETFCAP`, `KILL`, `AUDIT_WRITE`, `SYS_CHROOT`). Everything else stays dropped.

## `network`

```yaml
network:
  default: ask               # what a connection no rule covers does: ask (default) or deny
  dns: open                  # open (default) or restricted
  resolvers: [1.1.1.1]       # DNS servers the helper uses (default: this machine's)
  rules:
    - {to: api.anthropic.com, ports: 443, decision: allow}
    - {to: "*.github.com", ports: [22, 443], decision: allow}
    - {to: 10.20.0.0/16, ports: "5432, 5433", decision: allow}
    - {to: 1.1.1.1, ports: 53, proto: udp, decision: allow}
```

**The jail has no network unless you give it one.** With no `network` section, or `network: off`, it has only a loopback (`docker run --network none`). `network: open` gives it Docker's normal network, with nothing checked. A mapping with rules (below) checks everything that leaves the jail, over every protocol. **Rules need Docker and a one-time build of a small helper image (`aicatraz/egress:1`, Debian with nftables; it needs network access once).**

| Key | Meaning |
|---|---|
| `default` | For a connection no rule covers: `ask` (the human is asked, see below) or `deny`. |
| `dns` | `open`: the jail resolves every name except those a `deny` rule names. `restricted`: only names an `allow` or `ask` rule covers. With `open` a name you never mentioned can still carry data out inside a DNS query, as it can anywhere; use `restricted` to stop that. |
| `resolvers` | IPv4 addresses of the DNS servers to use. Default: the ones in this machine's `/etc/resolv.conf` (loopback stubs can't be used from a container, so name real ones if that is all you have). |
| `rules` | Checked top to bottom; the first rule that matches decides. |

**A rule**

| Key | |
|---|---|
| `to` | A host name (`api.example.com`), a pattern for subdomains (`*.example.com`, which does not include `example.com` itself), one IPv4 address, a CIDR range (`10.0.0.0/8`), or `*` for any destination. IPv6 is switched off in the jail. |
| `ports` | A list (`22, 80, 443`), a range (`1000-2000`), any mix (`22, 80, 443, 8000-8080`), or `*`. Written as a string or as a YAML list. Default: any port. |
| `proto` | `tcp`, `udp`, `icmp`, or a list. Default: `tcp` for a name or `*`, `tcp` and `udp` for an address. `udp` and `icmp` need an IP address or CIDR range as `to`. |
| `decision` | `allow`, `ask` or `deny`. `ask` works for TCP only. |

**How it is enforced.** A helper container (the sidecar) owns the jail's network. Its firewall rejects everything that is not allowed, and sends every TCP connection to a transparent proxy that asks the host; the jail has no network capability and can't change any of it. What the jail resolves goes through the helper's DNS server, which remembers which names resolved to which addresses, so a connection to an address can be judged, and shown to you, by name. A name rule allows an address only if the jail's own lookup of that name returned it. TCP goes through the proxy even when a rule allows it, so the jail sees a connection "open" before the decision falls; a refused one is reset on first use. UDP and ICMP are never asked about: they are rejected unless an address rule allows them.

**Private addresses.** Loopback, the private ranges (10/8, 172.16/12, 192.168/16), link-local (which includes the cloud metadata address 169.254.169.254), the shared range 100.64/10 and multicast can be allowed only by a rule that names an IP address or CIDR range. A host name, or `*`, never allows them, so DNS rebinding can't point an allowed name at your local network. Your own machine as seen from the jail (the Docker bridge address) is such an address.

**Asking.** A connection no rule covers (with `default: ask`) shows a prompt, in the `aicatraz up` overlay, a tmux popup or on `--approval-tty`, with the address, the name the jail looked up (if any), and these answers:

| Key | |
|---|---|
| `y` | Let this connection through; nothing is remembered. |
| `s` | Allow the target on the ports for this session. |
| `p` | Allow it and remember it (see below). |
| `x` | Block the target on the ports for this session. |
| `b` | Block the target on the ports and remember it (see below). |
| `e` | Edit the ports. They start as the port of the connection and can be a list, a range, a mix, or `*`: `22, 80, 443, 8000-8080`. |
| `t` | Pick another target for `s`, `p`, `x` and `b`: each name the jail looked up, or the address. |
| `n`, Enter, Esc | Deny. No answer before the approval timeout is a deny. |

Questions come one at a time; the next one sees the answer to the one before, so a burst of connections to one destination asks once. Without anybody to ask (no `up`, no `--approval-tty`), such connections are refused.

**Remembered rules** go to `network.json` in the project's state directory, after the rules of the config (a `deny` in the config always wins), and are picked up by running sessions. `aicatraz network list` shows both kinds, `aicatraz network forget <number> | --all` removes remembered ones, and `aicatraz network allow|deny <target> [--ports ...]` adds one without a prompt.

Every decision is in the audit log (`"event":"net"`).

## `ports`

```yaml
ports:
  - 3000                  # 127.0.0.1:3000 on this machine -> port 3000 in the jail
  - "8080:80"             # 127.0.0.1:8080 -> port 80
  - "127.0.0.1:9090:90"   # the address written out
  - "192.168.1.5:8000"    # address:port, same port in the jail
  - "8000-8010:8000-8010" # ranges of the same length
  - "5353:5353/udp"       # udp
```

Publishes ports of the jail on this machine, so that a browser (or anything else) here can reach a server that runs inside the jail. An entry is a port, `address:port`, `host:container`, or `address:host:container`, with an optional `/tcp` or `/udp` (tcp by default). **Without an address the port is published on 127.0.0.1 only**: this machine can connect, other machines can't. An address such as `0.0.0.0` publishes it to the network (`config check` warns).

- It needs a network: with no `network` section, or `network: off`, the jail has none and `ports` is an error. With `network: open` the jail publishes the ports itself; with rules the helper container does, and incoming connections are not subject to the rules (only what the jail connects to is).
- The server in the jail must listen on all addresses (`0.0.0.0`), not on `127.0.0.1`: a connection from outside arrives at the jail's own address, not at its loopback.
- Anything published is reachable by every program and every user on this machine, and by every web page open in your browser. The agent can start a server there too, and it can answer a request from a page you visit. Publish what you mean to, on the narrowest address.

## `devices`

```yaml
devices:
  - serial: "usb-Arduino_*"     # a name under /dev/serial/by-id, * allowed
  - usb: "2e8a:*"               # vendor:product, * allowed (here: every Raspberry Pi device)
  - usb: "1a86:7523"            # a CH340 serial adapter ...
    usb_serial: "A5069*"        # ... with this serial number
  - path: /dev/ttyUSB0          # a fixed device node; a glob is allowed
```

Gives the jail USB serial adapters, Arduino, ESP and Pico boards: hardware that comes and goes. Exactly one of `serial`, `usb` and `path` per entry; `usb_serial` goes with `usb`. `aicatraz devices` lists the USB devices of this machine with the vendor:product, serial number and device nodes to write here, and marks the ones the config hands to the jail.

- **What the jail gets:** the device nodes of the devices that match, at the same path as on the host (`/dev/ttyACM0`, `/dev/ttyUSB0`, `/dev/bus/usb/001/005` for tools that talk USB themselves), and the stable names under `/dev/serial/by-id/`. Use those: the number in `ttyACM0` changes between plug-ins. Nodes are owned by the jail user, mode 0660.
- **Plug and unplug:** the launcher watches the host. A device that appears gets its nodes in the running jail within a fraction of a second (devices already plugged in at start appear moments after the jail starts); one that disappears has them removed; one that comes back, under whatever name the kernel gave it, gets them again. A program that had the port open sees the usual error and has to open it again.
- **How:** the jail starts with device-cgroup rules for the classes `serial` and `usb` can match (ttyACM, ttyUSB, raw USB) and for the exact numbers of the nodes a `path` matches now. The nodes are made from outside by the launcher, with `docker exec --privileged` of our own binary, so it works on any image. The jail itself never gets `MKNOD` (not with `sudo: true` either), so a rule alone gives it nothing.
- **Limits:** a device of another class (`path`) has to be plugged in when the jail starts, since its rule can't be added later. No block devices: the mass-storage drive of a Pico in BOOTSEL mode, or anything that needs to be mounted, doesn't work; neither does anything that needs more than access to the node. For those, run the tool on the host through a rule (for example `picotool load` or a `make flash` that is `allow` or `ask`).
- **Not allowed:** the nodes that belong to the container (`/dev/null`, `/dev/pts/*`, ...) and `/dev/mem`, `/dev/kmem` and `/dev/port`. Not `path: /dev/*`.
- Every attach and detach is in the audit log (`"event":"device"`). Nothing is printed while the session runs, because the launcher's output shares the terminal with the program in the jail.

## `bar`

```yaml
bar:
  theme: brick          # a name from `aicatraz bar-colors`
  fg: white             # optional
  bg: "#102030"         # optional
  symbol: "⚠️"          # drawn before the alert of a waiting prompt; "" for none
  symbol_width: 2       # columns your terminal draws it in (default: a guess)
```

The look of the status bar of `aicatraz up` sessions in this project, to tell terminals apart at a glance. All keys are optional. `theme: random` takes a different named theme every time a session is attached. Colors are a name (`red`, `bright-blue`, `gray`), a number from 0 to 255, `#rrggbb`, or `default`. `symbol` is what the alert of a waiting prompt starts with (default `⚠️`; any text up to 8 characters, or `""` for none), and `symbol_width` is how many columns your terminal draws it in, 0 to 4. Terminals disagree about emoji: some draw `⚠️` two columns wide, some one. If the bar's right edge is off by one while an alert shows, set `symbol_width` to what you see. Without it AIcatraz guesses: 2 for an emoji, 1 for any other character. A flag overrides this, and this overrides the user's `AICATRAZ_BAR_*` variables, key by key. See [sessions.md](sessions.md#the-status-bar).

## `notifications`

```yaml
notifications:
  shim: true     # a `notify-send` in the jail shows a notification on your desktop (default: off)
  osc: true      # terminal notification sequences (OSC 9, 99, 777) reach your terminal (default: on)
```

How an agent's desktop notifications get out of the jail. Both can be set in the user config too.

`shim` puts a `notify-send` in the jail's `PATH`. The broker runs the host's own `notify-send` for it, and the jail sends text only: urgency, summary and body. Every other option (icons, hints, actions, timeouts) is dropped, control characters are removed, markup is escaped, the summary is cut at 100 characters and the body at 500, and the notification is labelled `AIcatraz <session>`. At most 5 in a burst, then one every 2 seconds. Each one is in the audit log. It needs `notify-send` and a desktop session on the host; there is no rule to write, and no D-Bus socket enters the jail. A `rules` entry for `notify-send` is ignored while `shim` is on.

`osc` is about the escape sequences some agents print to ask your terminal for a notification. In an `up` session (and so `run` and the agent commands in a terminal) `osc: false` drops them; `--no-session` runs have no layer to drop them. Through `tmux` they only reach your terminal if tmux passes them on.

## `limits`

| Key | Default | |
|---|---|---|
| `pids` | `1024` | Maximum number of processes in the jail. |
| `memory` | none | Memory limit, e.g. `512m` or `2g`. |

The jail always drops all capabilities, sets `no-new-privileges`, and has a read-only root filesystem. `/tmp`, `/var/tmp` and your (empty) home are docker volumes created for the session and removed when it ends, so they use disk, not RAM; there is no size limit beyond the free space of the Docker data directory. `/run` is a small tmpfs.

## `approval`

| Key | Default | |
|---|---|---|
| `timeout` | `2m` | How long a prompt waits before it counts as a deny, counted from when it is on your screen. 5 seconds to 1 hour. |
| `idle` | `5s` | In `aicatraz up`: how long you must have stopped typing before a prompt takes over the screen. A prompt that comes while you type waits, and the status bar blinks a message such as `⚠️ Network github.com:443  Ctrl-Q`; it opens when you have been idle that long, or at once when you press the info key (`Ctrl-Q`). `0` opens every prompt at once. Up to 10 minutes. |
| `guard` | `500ms` | How long a prompt that has just opened ignores keys. 100 milliseconds to 5 seconds. |

A prompt that waits for you to stop typing is denied after 10 minutes at most, plus its `timeout`. The tmux popup and `--approval-tty` don't wait: they show the prompt at once.

## `canary`

Checks, from inside the container, that every `hi` path is still hidden. A hidden *file* stops being hidden if something on the host replaces it by renaming a new file over it (most editors save that way).

| Key | Default | |
|---|---|---|
| `action` | `stop` | `stop`: warn loudly, refuse all host commands and end the session (exit code 70). `warn`: warn once and carry on. `off`. |
| `interval` | `3s` | How often to check (also checked before every host command). 200ms to 1h. |

## `global`

| Key | |
|---|---|
| `env_strip` | Glob patterns for variable names that are never passed to a host command, on top of the built-in set (`LD_*`, `PATH` from the container, `BASH_ENV`, `ENV`, `PYTHONPATH`, `NODE_OPTIONS`, `GIT_SSH_COMMAND`, `GIT_CONFIG_*`, `PERL5LIB`, `RUBYOPT`), which no config can override. |

## `rules`

Commands the agent may run on the host. **First matching rule wins; no match means deny.**

```yaml
rules:
  - command: make
    args: [deploy-test]
    decision: allow
    depends_on: auto
```

| Key | |
|---|---|
| `command` | The command name (not a path), or a glob such as `py*` or `"*"`. |
| `args` | Patterns, one per argument. Missing: any arguments. `[]`: no arguments. A final `*` matches all remaining arguments (including none); elsewhere `*` is a wildcard inside one argument and also matches `/`. |
| `deny_flags` | Flags the rule never grants. A call using one doesn't match this rule and falls through to later rules. Catches `--file=x`, abbreviations like `--fi`, `-f`, `-ffile` and bundled `-nf`. Exact `args` are stronger than `deny_flags`. |
| `decision` | `allow`, `ask`, `deny` or `local`. Required. |
| `depends_on` | Files the rule really executes, so changes can be noticed: `auto`, or a list of globs relative to the working directory, with `{args[N]}` for an argument. |
| `env_pass` | Names of variables from the jail to pass to the command, if the container sent them. Never `LD_*`, `PATH`, `BASH_ENV` and the other built-in names. |
| `confine_paths` | `true`: refuse the call if any argument that is or points to a path resolves outside the mounted volumes or into a hidden path. |

**Decisions**

- `allow`: runs on the host without asking.
- `ask`: you are asked first (an overlay in `aicatraz up`, a popup in `up --tmux`, or `--approval-tty`); with none of them, it is denied. In `up`, a question that comes while you are detached waits (until the approval timeout) and is shown when you attach. Answers: `y` run once, `s` run and allow this exact call (same arguments, directory and file contents) for the session, `p` run and remember the shown file versions (only offered when files changed), anything else denies; timeout denies.
- `local`: the shim runs the jail's own binary. No prompt, no pins, nothing leaves the jail. This is how arguments choose between jail and host: `make build` local, `make deploy` on the host.
- `deny`: refused. Useful as a carve-out in front of a broader rule, or as an explicit last line.

The host command runs as you, in the directory the agent was in (which must lie inside a mounted volume and not in a hidden path), with a freshly built environment: a small base set (`PATH` from the broker, `HOME`, `USER`, `LANG`, `TZ`, `TERM`) plus the rule's `env_pass`. The binary is found on the broker's `PATH`; one that lives in a writable volume is never used.

`aicatraz pins list` shows what the project has approved and whether each file has changed since; `aicatraz pins diff [<path>...]` shows the changes (file contents are shown escaped); `aicatraz pins forget <path>... | --missing | --all` takes pins away, so the next run asks again. Forgetting is always safe: it only makes things stricter.

**`depends_on: auto`** knows these sets: `make` (`Makefile`, `GNUmakefile`, `*.mk`, and files named by `include`), `npm`/`npx`/`pnpm`/`yarn` (`package.json`, lockfiles, `.npmrc`), `git` (`.git/config`, `.git/hooks/*`, `.gitattributes`, `.gitmodules`), script interpreters (`python*`, `bash`, `sh`, `node`, `ruby`, `perl`, `php`: the script argument), `docker` (`Dockerfile*`, compose files). For other commands list the files yourself. Pins are kept per project, outside every mount; only your `p` answer ever updates one. More than 200 dependent files can't be pinned, so such a rule asks every time.

**The linter** warns about: a rule that an earlier one already covers (never reached), `command: "*"` with `allow`, shells/interpreters/`env`/`xargs`/`ssh`/`find`/`sudo` allowed with unrestricted arguments, `allow` on `make`/`npm`/`git`/interpreters/`docker` without `depends_on`, secret-looking `env_pass` on a rule that runs without asking, `depends_on` or `env_pass` on a `local` or `deny` rule, `depends_on: auto` on a command without a built-in set.

## `agent`

| Key | Default | |
|---|---|---|
| `tool` | none | `claude` or `opencode`. Used by `aicatraz agent`, and to choose the version. |
| `version` | `latest` | `latest` (the installed current one) or a pinned version, installed on first use. |
| `skip_permissions` | `true` | Start the agent in its skip-permissions mode: `--dangerously-skip-permissions` plus `skipDangerousModePermissionPrompt` for Claude Code, `OPENCODE_PERMISSION={"*":"allow"}` for opencode. The jail is the boundary. |

Your agent settings are copied into per-project directories on every launch; the agent's own history stays there between launches. See [agents.md](agents.md) for exactly what is copied.
