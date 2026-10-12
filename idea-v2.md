# AIcatraz

A jail for AI agents on a Linux dev machine. The agent gets a Debian container with full freedom inside it, and a narrow, policy-controlled door for the few things that must run on the host.

## Goal and non-goals

**Goal: scope containment.** The agent can only read and write what you explicitly mounted. Anything that must happen on the host (deploys, git push, publishing) goes through a broker that applies a policy you wrote, outside the agent's reach.

**Non-goals**
- Preventing exfiltration of what the agent can legitimately see. Outbound network stays open because the agent needs its API.
- Non-Linux hosts.
- Defending against kernel or container-runtime escapes. Docker's isolation is assumed.

**The rule the design is built around:** the container is untrusted, the host broker is trusted. Everything the container sends (argv, env, cwd, binary name) is a *request*. The broker decides, resolves and builds everything itself. The shim in the container is a convenience, never a security boundary.

## Parts

| Part | Where | Job |
|---|---|---|
| `aicatraz` launcher | host | reads the config, sets up the tmux session, starts the broker and the container |
| broker | host, one per session | owns the socket, applies policy, runs commands, tracks file pins, writes the audit log |
| shim | container | one small program, symlinked under the names of proxied commands, forwards the call to the broker |
| config | host, outside every mounted volume | policy and mounts; the agent can never edit it |

The broker lives and dies with the session. No long-running daemon, nothing to supervise. If the broker exits, the container is stopped.

## The container

### Image: `Dockerfile.az`

The container is per project. A project that already has a production container starts from that, and the jail is layered on top at run time. Nothing is baked into the image.

```yaml
image:
  dockerfile: Dockerfile.az     # default if the file exists
  context: .
# or:  image: debian:13        # plain reference, no build
# neither given: built-in default Debian image
```

- `Dockerfile.az` can be one line (`FROM debian:latest`) or a whole production environment.
- `docker build` runs on every `up`; Docker's layer cache keeps it quick. The image is tagged per project.
- The image's `ENTRYPOINT` and `CMD` are replaced by AIcatraz's own init. A production entrypoint would start the app, not the agent.
- **UID:** the container runs with `--user <host uid>:<gid>`, whatever the image's `USER` says. A generated `/etc/passwd` and `/etc/group` are bind-mounted read-only so `whoami` and `$HOME` work in images that don't know that UID.
- **The shim is mounted, not built in.** The launcher generates a per-session directory of symlinks named after the commands in the rules (`make`, `git`, ...) and mounts it read-only together with the binary, and prepends it to `PATH` (`/aicatraz/bin`). No second image, no rebuild when rules change, same binary version as the broker, and it works on any base image since the binary is static. The agent can override `PATH`; that's fine, the shim is not a boundary.
- Host binaries for the toolchain can still be bind-mounted read-only via `volumes`.

**Build safety**
- The agent can edit `Dockerfile.az`, and the host builds it on the next launch. The pin mechanism (see below) covers it: if `Dockerfile.az` or any file in the build context it references changed since you last approved, `up` shows the diff and asks before building. Mounting it `ro` is the stricter alternative.
- **Hidden files must not enter the build context.** `COPY . /app` would bake `.env` into the image where the agent can read it. The launcher refuses to build when a `hi` path isn't excluded by `.dockerignore`.

### Hardening and runtime
- Hardening by default: `--cap-drop=all`, `--security-opt no-new-privileges`, read-only root filesystem with tmpfs for `/tmp` and `/run`, pids and memory limits. No Docker socket, never `--privileged`.
- Same UID/GID as the host user, so files in rw volumes keep sane ownership.
- **Same absolute path for every mount** on host and in the container, so `$PWD` means the same on both sides.
- **`~` resolves, but is empty.** `$HOME` is the host user's real home path, but inside the container that directory is a fresh tmpfs. No dotfiles, no SSH keys, no shell history. Any `~/...` string the agent passes to a proxied command resolves to the real path by plain string equality.
- Inside the container the agent can do anything, including `sudo`. With `no-new-privileges` and dropped capabilities that is harmless.

### Volumes

Compose style: `path:mode`, `~` expanded on the host, same path on both sides.

```yaml
volumes:
  - ~/git/myproject:rw
  - ~/usr/local/bin/mytool:ro
  - ~/git/myproject/.env:hi        # hi = hide
```

| Mode | Effect inside the container |
|---|---|
| `rw` | read/write |
| `ro` | read-only |
| `hi` | hidden: a file reads as empty, a directory reads as empty |

**Nested entries override their parent.** A `ro` or `hi` entry inside an `rw` entry is a mount placed on top of it.
- `hi` on a file: `/dev/null` is bind-mounted over it. It reads as empty and can't be deleted or renamed by the agent (`EBUSY`).
- `hi` on a directory: an empty read-only tmpfs is mounted over it.
- `ro` on a path inside an `rw` mount: a read-only bind mount over it. Useful for `.git/hooks`, `.git/config`, CI files.

Rules the launcher enforces:
- A nested entry must be inside a mounted parent, and its target must exist at launch. A missing `.env` is an error, not a silent skip, because otherwise the agent could create it.
- Hidden paths are still visible to **proxied host commands**: `make deploy` can read `.env`, the agent can't. That is the point.

**Known weak spot for single-file hides.** A bind mount sits on a specific inode. If an editor on the host saves `.env` by writing a new file and renaming it over the old one, the name now points at a new inode, nothing is mounted over it, and the agent can read the real contents. Two defences: prefer hiding a **directory** (`.secrets/`, `config/private/`), where file replacement inside it doesn't matter; and the broker runs a canary check (inode of every `hi` file as seen from inside the container) at each command and on a timer, and freezes the session with a loud warning if one stops being hidden.

### Container environment
- `env.static`: name and value, set in the container.
- `env.from_host`: names only, value copied from the host at launch.
- The launcher warns on names that look like secrets (`*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`), because the agent sees these and has network access.

## Running host commands

### Flow
1. Agent runs `make deploy`. `make` in the container is the shim (a symlink to it).
2. Shim connects to the broker socket and sends: command **name** (not a path), argv, cwd, stdin, terminal size, and the env vars it would like to pass.
3. Broker matches the request against the policy. No match means denied.
4. If the rule says `local`, the broker tells the shim to run the container's own binary, and nothing else happens on the host.
5. Otherwise the broker checks the rule's dependent files (see below), which can turn `allow` into `ask`.
6. If the result is `ask`, the broker asks you.
7. Broker resolves the binary itself from the host, runs it in a pty with a freshly built environment, and streams output, signals and the exit code back.

The shim doesn't know what is allowed. Adding a command means adding one symlink in the image.

### Socket
- Only the socket file is bind-mounted, not its directory.
- The broker checks `SO_PEERCRED` and rejects peers that aren't in the container.
- The shim reconnects with exponential backoff; broker and shim exchange heartbeats. If the shim vanishes, the broker kills the child process group. If the broker vanishes, the shim exits with a clear error instead of hanging.

### Policy

First matching rule wins, default is deny. Rules match the argument vector as **tokens**, not a flat string, so `make *` can't be bent by `make -f /somewhere/evil.mk`.

```yaml
global:
  env_strip: [AWS_*, GITHUB_TOKEN]       # always removed, on top of the built-in set

rules:
  - command: make
    args: [deploy-test]                   # exact tokens
    decision: allow
    depends_on: auto                      # preset for make, see below

  - command: make
    args: [deploy-prod]
    decision: ask
    depends_on: auto

  - command: make
    args: ["*"]
    deny_flags: [-f, --file, -C, --directory, -I, --include-dir]
    decision: ask
    depends_on: auto

  - command: python
    args: ["scripts/*.py"]
    decision: allow
    depends_on: [ "{args[0]}", requirements.txt ]

  - command: git
    args: [push, "*"]
    decision: ask
    env_pass: [SSH_AUTH_SOCK]

  - command: "*"
    decision: deny
```

- `decision`: `allow`, `ask`, `deny` or `local`. A `deny` rule in front of a broader rule is the carve-out mechanism.
- **`local` runs the command inside the container**, with the container's own binary. This lets arguments decide where a command runs: `make build` locally, `make deploy` on the host. The broker still makes the decision; it tells the shim "run local" and the shim `exec`s the real binary it finds on `PATH` past `/aicatraz/bin`. `local` needs no prompt, no pins and no environment filtering, since nothing leaves the container. No matching rule is still a deny.

```yaml
  - command: make
    args: [build, test, lint]
    decision: local                       # runs in the container
  - command: make
    args: [deploy-test]
    decision: allow                       # runs on the host
```
- `cwd` must `realpath` into a mounted volume.
- Optional `confine_paths: true` rejects any argument that resolves outside the mounted volumes.

### Dependent files: pinning what a rule really executes

An allow rule for `make deploy-test` really means "run whatever the Makefile says", and the agent can write the Makefile. So a rule can name the files whose content it depends on, and the broker pins them.

```yaml
depends_on: auto                          # built-in preset for this command
depends_on: [Makefile, "mk/*.mk"]         # explicit globs, relative to cwd
depends_on: [ "{args[0]}" ]               # a path taken from the call itself
```

**How it works**
1. The broker keeps a **pin store** on the host, outside every mounted volume, **per project**: path, sha256 and a stored copy of the content (size-capped) for the files of each rule. A project is identified by the `realpath` of its config file, so the same Makefile in two projects is pinned and approved separately, and approvals never carry over. The store lives under `~/.local/state/aicatraz/<project-id>/pins/`.
2. On a request, the broker re-hashes the rule's dependent files and compares to the pin.
3. All hashes match: the rule's decision stands, so `allow` stays `allow`.
4. Any file is new, changed or gone: **the decision becomes `ask`**, and the prompt carries a prominent warning and the unified diff against the pinned version. For a first-ever run there is no pin; the prompt says "never approved before".
5. When you approve, you choose **approve once** or **approve and re-pin**. Only a human answer ever updates the pin store; the agent can never move a pin.

`ask` rules get the same check and the same warning, so you always see what changed since you last looked, even though you are asked anyway.

**Presets for `depends_on: auto`**

| Command | Files pinned |
|---|---|
| `make` | `Makefile`, `makefile`, `GNUmakefile`, `*.mk`, plus files named by `include` found while hashing |
| `npm`, `pnpm`, `yarn` | `package.json`, lockfile, `.npmrc`, scripts it names |
| `git` | `.git/config`, `.git/hooks/*`, `.gitattributes` |
| `python`, `bash`, `sh`, `node` | the script argument itself |
| `docker`, `docker compose` | `Dockerfile`, `compose*.y*ml` |

Presets are a starting point; for commands outside the table you write the list yourself. The linter warns when an `allow` rule on a known workspace-executing command has no `depends_on`.

**Honest limits**
- The pin only covers files you list (or the preset knows). A Makefile that calls `./build.sh` is covered only if `build.sh` is in the list. Keep `depends_on` generous: `scripts/**` is cheap.
- There is a small window between hashing and the executed program reading the file. The broker re-hashes immediately before `exec`, which closes the long gap but not the tiny one. For stronger guarantees, mount the pinned files `ro` (e.g. `~/git/myproject/Makefile:ro`); then the agent can't change them at all and the pin store is just a backstop.
- Hidden files (`hi`) are read by the broker from the real path and can be pinned too.

### Environment for proxied commands: whitelist, not blacklist
The broker builds the env itself:

1. A small base set from the host (`PATH` from the broker's own trusted value, `HOME`, `USER`, `LANG`, `TERM`, `TZ`).
2. Plus `env_pass` entries from the matching rule, only if the container actually sent them.
3. Minus `global.env_strip`.
4. Minus a hardcoded never-pass set the config cannot override: `LD_*`, client-supplied `PATH`, `BASH_ENV`, `ENV`, `PYTHONPATH`, `NODE_OPTIONS`, `GIT_SSH_COMMAND`, `GIT_CONFIG_*`, `PERL5LIB`, `RUBYOPT` and similar.

New variables are denied by default.

### Terminal layout and approval

The agent and the approval prompt must be on separate ptys so the agent can't type into or fake a prompt. Approach: **tmux**, driven by the launcher.

- `aicatraz up` starts a tmux session. Pane 1 holds the agent (`docker run -it`). The broker runs as a second process of the session.
- An `ask` prompt appears as a **`tmux display-popup`** over the agent pane. It is a separate pty owned by the host-side tmux client, so the agent's keystrokes and output can't reach it, and the container has no access to the tmux socket. The agent keeps running underneath.
- Fallbacks: a dedicated approval pane, or `--approval-tty /dev/pts/N`.
- Prompt content: the full resolved command (real binary path, argv, cwd, env differences), the matching rule, and the **file-change warning with diff** when a pin differs.
- Answers: allow once, allow once and re-pin, allow for this session (this exact argv; memory only, never written to the config), deny. Timeout is deny.
- A built-in multiplexer is a possible later step. tmux is the cheap, robust first version.

### Execution details
- `subprocess` with `shell=False`, never through a shell string.
- Pty with SIGINT/SIGTERM forwarding and `SIGWINCH` resize.
- Concurrency limit per session; each command in its own process group.
- Everything runs as the host user. The broker never escalates.

## Agent entrypoints

`aicatraz claude`, `aicatraz opencode` and similar start a known agent inside the jail with no manual setup. Each agent has a built-in profile.

**Installation.** Agents are not installed into the image (production bases may be Alpine or distroless). A throwaway helper container installs the tool into a host cache, `~/.local/share/aicatraz/tools/<tool>/<version>`, and it is mounted **read-only** into the jail. Updates are explicit (`aicatraz tools update`) and versions can be pinned. This works best for self-contained binaries; a tool needing a particular libc can be a problem on musl bases.

**Config mapping.** The real home stays empty, so the agent's config is provided on purpose:
- A profile lists which files and directories the agent needs. Each profile entry is either `sync` or `persist`.
- `sync` entries (settings, instructions, skills, credentials) are **copied from your real config into a per-project state directory on every launch**, overwriting what is there. Changes you make to your config carry over; anything the agent changed in them is discarded.
- `persist` entries (history, sessions, memory) live in the per-project state directory (`~/.local/state/aicatraz/<project-id>/home/`) and are kept between launches.
- That directory is mounted rw at the agent's config path. Your real config is never mounted, so the agent can't alter it.
- Credentials are copied like any other `sync` file, so they are visible to everything in the jail. API keys through `env.from_host` are the alternative.
- Settings that point at host paths or host commands (hooks, MCP servers) will run inside the container, or fail there. Note this in the docs.

**Permission prompts.** The jail is the security boundary, so the agent's own prompts are redundant. The entrypoint starts the agent in its skip-permissions mode by default (`--dangerously-skip-permissions` for Claude Code, the equivalent for others), and `agent.skip_permissions: false` turns it off.

```yaml
agent:
  tool: claude
  version: latest                # or a pinned version
  skip_permissions: true
```

**Sessions and `aicatraz resume`.** The agents keep their conversation history in `persist` entries, so the transcripts already survive between launches. AIcatraz adds the index on top.
- **Where:** a per-project `sessions.json` next to the state directory (`~/.local/state/aicatraz/<project-id>/sessions.json`), not inside the mounted `home/`, so the agent can't edit it.
- **What is recorded:** tool, session id, start and end time, exit code, a short title or first prompt when the agent's store has one, and the image tag.
- **How the id is found, in order of preference:** (1) where the tool accepts a chosen id up front (Claude Code's `--session-id`), AIcatraz generates it and knows it from the start; (2) otherwise it compares the agent's session store in the state directory before and after the run and takes the new or modified session; (3) scraping the exit message is only a fallback, since that text is fragile.
- **Commands:** `aicatraz sessions` lists them for the project; `aicatraz resume` picks the most recent one (asks if the tool is ambiguous); `aicatraz resume <id>` picks a specific one; `aicatraz resume --pick` shows a list. It starts the jail as usual and passes the id to the tool's own resume option (`--resume <id>` for Claude Code, `--session <id>` for opencode; flags to be confirmed against the installed versions in M5).
- The id comes from a file the agent can write, so it is validated (`[A-Za-z0-9_-]+`, length-limited) and passed as one argv token, never through a shell.
- Same-path mounts help here: Claude Code keys its transcripts by working directory, and the directory is identical on every launch.
- If the config file moves, the project id (derived from its `realpath`) changes and the history looks lost. An optional `project.id` in the config pins the id explicitly.

## Audit log
Append-only file owned by the host user, outside every mounted volume. One JSON line per request: timestamp, argv, cwd, matched rule, pin status (ok / changed / new, with file list), decision and who made it, exit code, duration. Denied requests are logged too; that's where an agent probing the policy shows up.

## Known risks, stated plainly
- **Allowed commands that execute workspace content** are now mitigated by pins, not removed. A change to any pinned file turns the rule into an `ask`; unpinned files a command reaches are still blind spots.
- **Planted files in the rw workspace** (`.git/hooks`, `.envrc`, `.vscode/tasks.json`, rc files) run when *you* later use the project outside the jail. Mount the dangerous ones `ro`, and review diffs before running anything.
- **Symlinks in the workspace** are harmless to the container but can mislead host-side tools. Mitigated by `realpath` checks on cwd and `confine_paths`.
- **Container-visible secrets** (`env.from_host`) can leave over the open network. Keep that list short.
- **Open network** is intentional. A later option is an egress proxy with a domain allowlist.

## Order of work
1. Launcher plus hardened container: `Dockerfile.az` build, same-path mounts, `rw/ro/hi` volumes, empty `~`. Already useful alone.
2. Broker and shim: token-based policy with `allow`/`deny`/`local`, whitelist env, audit log.
3. tmux integration and the `ask` flow with session grants.
4. Pin store, `depends_on`, presets, diff in the prompt; also covers `Dockerfile.az`.
5. Agent entrypoints (`aicatraz claude`, ...): tool cache, config sync, skip-permissions.
6. Canary check for hidden files, config linter, `confine_paths`.
7. Optional egress allowlist, built-in multiplexer.

## What changed from the first idea and why
- **Kept:** Docker jail, scope containment as the goal, unix-socket proxy, a wrapper that doesn't decide, allowlist with first-match-wins, `allow`/`ask` per command, same-path mounts, static and from-host container env, heartbeat and retry, pty execution.
- **Changed:** compose-style volumes; the broker builds the environment from a whitelist; the broker resolves the binary; argv is matched as tokens with flag denies; approval is a tmux popup on a separate pty; the config lives outside the workspace; one broker per session.
- **Added:** per-project image from `Dockerfile.az` with the shim mounted at run time, a `local` decision so arguments choose between container and host, agent entrypoints with config sync, `hi` mode and nested overrides, dependent-file pinning with diffs, audit log format, peer-credential check, hardening flags, linter, explicit risk list.
