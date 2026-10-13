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

**Writes to a hidden file are accepted and discarded.** The file is `/dev/null` underneath, so `echo x > .env` in the jail succeeds and changes nothing. Deleting or renaming it fails with `EBUSY`.

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
- The socket lives in a private per-session directory under `$XDG_RUNTIME_DIR/aicatraz/<launcher pid>/` (or `/tmp/aicatraz-<uid>/`), because unix socket paths are limited to about 100 bytes. Only the socket file is bind-mounted, read-only, not its directory.
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
- **Argument matching.** `args` is a list of patterns, one per argument. A missing `args` key matches any arguments; `args: []` matches only "no arguments". A *final* `*` matches all remaining arguments (including none); everywhere else `*` is a wildcard inside a single argument and also matches `/`. So `[deploy-test]` is exact, `[push, "*"]` is `git push` with anything after it.
- **`deny_flags` make the rule not match** (the request then falls through to later rules and finally to the default deny). Matching leans towards "denied" when unsure: `--file`, `--file=x`, abbreviations such as `--fi`, `-f`, `-ffile` and bundled `-nf` all count as `-f`/`--file`. Exact `args` lists are the stronger tool; `deny_flags` is for broad catch-all rules.
- `cwd` must `realpath` into a mounted volume (not into a hidden one).
- The binary is looked up on the **broker's** `PATH` (absolute entries only) and a candidate that lives in a writable volume is skipped, so a planted `make` can't be run on the host.
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
5. When you approve, you choose `y` **run once** (the pin stays as it was, so the next call asks again), `p` **run and remember** (the shown versions become the pin), or `s` for a session grant. Only the `p` answer from a human ever updates the pin store; the agent can never move a pin, and repeating a call never creates trust. What is pinned is the snapshot the prompt showed, not a fresh read.
6. Right before the command starts the broker checks the files again; if they changed while the request was being processed (for example while the prompt was open) the call is denied.

`ask` rules get the same check and the same warning, so you always see what changed since you last looked, even though you are asked anyway.

Details worth knowing:
- Pins are per file, not per rule. A file you approved while reviewing one rule counts as approved for every rule of the project that depends on it.
- A glob also notices a **new** file that matches and a pinned file that **disappeared**.
- A session grant (`s`) is tied to the exact contents of the dependent files at that moment. If a file changes, the grant no longer applies.
- `{args[N]}` is filled with the argument taken literally (`*` in an argument is not a wildcard), and every dependent file must be inside a mounted volume and not inside a hidden path, otherwise the call is denied. A rule with `depends_on` but no pin store is denied, never run unchecked.
- More than 200 dependent files can't be pinned: such a rule asks every time, without the `p` option.
- A file larger than 256 KiB is pinned by hash; the prompt then says that no diff is available.

**Presets for `depends_on: auto`**

| Command | Files pinned |
|---|---|
| `make` | `Makefile`, `makefile`, `GNUmakefile`, `*.mk`, plus files named by `include` found while hashing |
| `npm`, `npx`, `pnpm`, `yarn` | `package.json`, the lockfiles, `.npmrc`, `.yarnrc*` |
| `git` | `.git/config`, `.git/hooks/*`, `.gitattributes`, `.gitmodules` (repository root found upwards from the working directory) |
| `python*`, `bash`, `sh`, `dash`, `zsh`, `node`, `ruby`, `perl`, `php` | the script argument itself (none for `-c`, `-e`, `-m`) |
| `docker`, `docker-compose` | `Dockerfile`, `Dockerfile.*`, `compose*.y*ml`, `docker-compose*.y*ml`, `.dockerignore` |

Presets are a starting point; for commands outside the table you write the list yourself (`aicatraz config check` warns when `depends_on: auto` is used on a command without a preset). The "no `depends_on` on a workspace-executing rule" warning arrives with the linter in M6.

**Honest limits**
- The pin only covers files you list (or the preset knows). A Makefile that calls `./build.sh` is covered only if `build.sh` is in the list. Keep `depends_on` generous: `scripts/**` is cheap.
- There is a small window between hashing and the executed program reading the file. The broker re-hashes immediately before `exec`, which closes the long gap but not the tiny one. For stronger guarantees, mount the pinned files `ro` (e.g. `~/git/myproject/Makefile:ro`); then the agent can't change them at all and the pin store is just a backstop.
- A file in a hidden path can **not** be a dependency: the broker would otherwise read it and put its content into a prompt. Such a rule is denied.

**The image build is pinned the same way.** The files that define it (`Dockerfile.az`, the ignore files, and single files named by `COPY`/`ADD`) are checked before the launcher builds. If one is new or changed, the launcher shows the diff and asks on its own terminal (the agent isn't running yet), with `y` build once, `p` build and remember, `n` cancel. Without a terminal it refuses and says so; `--approve-build` pins non-interactively. Directories copied wholesale (`COPY . /app`) are not pinned, since that is the project's own code, which the agent legitimately edits.

### Environment for proxied commands: whitelist, not blacklist
The broker builds the env itself:

1. A small base set from the host (`PATH` from the broker's own trusted value, `HOME`, `USER`, `LANG`, `TERM`, `TZ`).
2. Plus `env_pass` entries from the matching rule, only if the container actually sent them.
3. Minus `global.env_strip`.
4. Minus a hardcoded never-pass set the config cannot override: `LD_*`, client-supplied `PATH`, `BASH_ENV`, `ENV`, `PYTHONPATH`, `NODE_OPTIONS`, `GIT_SSH_COMMAND`, `GIT_CONFIG_*`, `PERL5LIB`, `RUBYOPT` and similar.

New variables are denied by default.

### Terminal layout and approval

The agent and the approval prompt must be on separate ptys so the agent can't type into or fake a prompt. Approach: **tmux**, driven by the launcher.

- `aicatraz up` starts a tmux session (`aicatraz-<project>`) whose single pane runs `aicatraz run`; if the session already exists it attaches to it. Inside tmux already, `up` is just `run`. The broker lives inside that `run` process, so there is no separate broker pane. The launcher's environment reaches the pane through a private `0600` file that is deleted on use, never through tmux's command line (it may contain `env.from_host` secrets).
- An `ask` prompt appears as a **`tmux display-popup`** over the agent's pane. It is a separate pty owned by the host-side tmux client, so the agent's keystrokes and output can't reach it, and the container has no access to the tmux socket. The agent keeps running underneath.
- `aicatraz shell` opens another shell in the running jail (`docker exec`) from a second terminal. It uses the same broker, so host commands and prompts work there too.
- Fallback: `--approval-tty /dev/pts/N` shows prompts on a terminal you name. Run something idle there (`sleep infinity`), not a shell that would compete for the keystrokes.
- With neither, `ask` rules are denied, and the launcher and the denial message say how to fix it.
- **The prompt is hardened.** Everything in it comes from the agent, so arguments are shown quoted with control characters, escape sequences and non-ASCII turned into visible escapes (an argument can't redraw the prompt or fake a second one); long values are cut and say so. A single key answers it; only `y` and `s` approve, every other key (Enter, Esc, Ctrl-C, ...) is ignored or denies. Keys typed before the prompt appeared are discarded, so type-ahead can't answer it.
- While waiting, the broker and shim keep exchanging heartbeats. If the agent's command is interrupted, the prompt is withdrawn. At most 4 prompts can wait at once.
- Prompt content: the full resolved command (real binary path, argv, cwd, passed environment variable names), the matching rule, and the **file-change warning with diff** when a pin differs (M4).
- Answers: `y` run once, `s` run and allow this exact call (same arguments, same directory) again for the rest of the session (memory only, never written to the config), anything else deny. Timeout is deny. `approval.timeout` in the config sets it (default 2 minutes, 5 seconds to 1 hour). Re-pinning arrives with M4.
- A built-in multiplexer is a possible later step. tmux is the cheap, robust first version.

### Execution details
- `subprocess` with `shell=False`, never through a shell string.
- Pty with SIGINT/SIGTERM forwarding and `SIGWINCH` resize.
- Concurrency limit per session; each command in its own process group.
- Everything runs as the host user. The broker never escalates.

## Agent entrypoints

`aicatraz claude` and `aicatraz opencode` start a known agent inside the jail with no manual setup. `aicatraz agent` starts the one named by `agent.tool` in the config; `aicatraz up --agent claude` does the same inside the tmux session. Arguments after `--` go to the agent.

```yaml
agent:
  tool: claude
  version: latest                # or a pinned version, e.g. 2.1.296
  skip_permissions: true
```

**Installation.** Agents are not installed into the image. The first use installs the agent with its official installer inside a throwaway Debian container (so the downloaded script never touches the host) and caches the executable at `~/.local/share/aicatraz/tools/<tool>/<version>/`. That directory is mounted **read-only** at `/aicatraz/tools`, which is on the jail's `PATH`.
- `aicatraz tools list`, `tools install <tool> [version]`, `tools update [tool]`. A pinned `agent.version` uses exactly that cached version (installing it on first use). `tools install <tool> --from-host` copies the executable you already have on `PATH` instead (no network).
- Both agents are dynamically linked against glibc, so the image has to be Debian/Ubuntu-based. On an Alpine image the launcher says so instead of failing with a bare "no such file".
- Both agents' own auto-update is switched off.

**Configuration.** Your real home is never mounted. Each agent has a profile that says which configuration it gets. The agent's config directories exist at the same paths as on the host (`~/.claude`, `~/.config/opencode`, ...) but are backed by per-project directories in `~/.local/state/aicatraz/<project>/agents/`.
- **Synced** entries are copied from your real config on every launch and overwrite what the jail has. A change you make carries over; anything the agent changed in them is discarded; a file you delete disappears from the jail. Files that haven't changed (size, mode, time) are not copied again.
- **Seeded** entries are copied only if the jail doesn't have them yet.
- Everything else in those directories is the agent's own state (history, transcripts, caches) and survives between launches, per project. Your host's transcripts and history are **not** copied in, and the agent's state never reaches your real files.

| Agent | Synced from your home | Own state in the jail |
|---|---|---|
| Claude Code | `settings.json`, `CLAUDE.md`, `keybindings.json`, `.credentials.json`, `~/.claude.json` (placed inside the config directory), `skills/`, `agents/`, `commands/`, `plugins/` | everything else under `~/.claude` (`projects/`, `history.jsonl`, ...) |
| opencode | `opencode.json(c)`, `AGENTS.md`, `agents/`, `commands/`, `plugins/`, `skills/`, `tool/`, `themes/`; `auth.json`, `mcp-auth.json`; `kv.json` (seeded) | the database, snapshots, logs, `node_modules` |

- Claude Code is told where to look with `CLAUDE_CONFIG_DIR`, which also keeps `~/.claude.json` inside the mounted directory (a single file can't be bind-mounted safely because the agent rewrites it by renaming).
- opencode's database can be many gigabytes (11 GB on the machine this was built on), so it is never copied: the jail starts with an empty one per project.
- Symlinks in your config are followed (a symlinked skills directory is copied as what it points to) and nothing in the copy is a symlink. A single entry may copy at most 256 MB.
- Credentials are copied like any other synced file, so everything in the jail can read them. Settings that call host paths or commands (hooks, MCP servers) run inside the container or fail there.
- The parent directories of the mounted roots (`~/.config`, `~/.local/share`) are created owned by the user, so tools can still write next to them.

**Permission prompts.** The jail is the security boundary, so the agent's own prompts are redundant. By default the agent starts in its skip-permissions mode: `--dangerously-skip-permissions` for Claude Code (plus `--settings {"skipDangerousModePermissionPrompt":true}`, because the flag alone makes Claude ask for a confirmation on every start; the setting is passed on the command line so your own `settings.json` is not touched), `OPENCODE_PERMISSION={"*":"allow"}` for opencode. `agent.skip_permissions: false` turns it off.

**Sessions and `aicatraz resume`.** Transcripts already survive between launches. AIcatraz adds an index on top.
- Where: `~/.local/state/aicatraz/<project>/sessions.json`, not inside the mounted directories, so the agent can't edit it. It records tool, id, title, start and end time, exit code and image.
- Finding the id: for Claude Code, AIcatraz generates the id and passes `--session-id`; the session is recorded only if a transcript for it exists afterwards (an empty session can't be resumed). For opencode, which picks its own ids, the launcher asks opencode itself (`session list --format json`, in a throwaway container with the same mounts) which session in this directory was just touched.
- On exit the id is announced: `session <id> saved; continue it with aicatraz resume`.
- `aicatraz sessions` lists them, most recent first. `aicatraz resume` continues the most recent one, `aicatraz resume <id>` a specific one, `aicatraz resume --pick` shows a numbered list. It starts the jail as usual and passes the id to the agent's own option (`--resume <id>` for Claude Code, `--session <id>` for opencode).
- Ids come from files the agent can write, so they are validated (`[A-Za-z0-9_-]`, at most 100 characters) and passed as one argument, never through a shell.
- The project is identified by the `realpath` of the config file, so moving it makes the history look lost. `project.id` in the config pins the id explicitly.

## Audit log
Append-only file owned by the host user, outside every mounted volume. One JSON line per request: timestamp, argv, cwd, matched rule, pin status (ok / changed / new, with file list), decision and who made it, exit code, duration. Denied requests are logged too; that's where an agent probing the policy shows up.

## Known risks, stated plainly
- **Allowed commands that execute workspace content** are now mitigated by pins, not removed. A change to any pinned file turns the rule into an `ask`; unpinned files a command reaches are still blind spots.
- **Planted files in the rw workspace** (`.git/hooks`, `.envrc`, `.vscode/tasks.json`, rc files) run when *you* later use the project outside the jail. Mount the dangerous ones `ro`, and review diffs before running anything.
- **Check-then-run window on the working directory.** The broker resolves and checks `cwd`, then starts the command. In that moment the agent could swap a directory in the workspace for a symlink pointing elsewhere. The command still has to be one the policy allows, and it runs as you in a different directory, so the impact is limited, but it is not zero. Mounting sensitive trees `hi` or `ro` removes the option.
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
