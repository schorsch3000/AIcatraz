# How AIcatraz works

The design as it stands, with the reasoning. For using it see the [README](../README.md) and the [configuration reference](configuration.md); for what it does and does not protect against see [security.md](security.md). The original idea and the first design were folded into this page, and the agent entrypoints into [agents.md](agents.md).

## Goal and non-goals

**Goal: scope containment.** The agent can only read and write what you explicitly mounted. Anything that must happen on the host (deploys, git push, publishing) goes through a broker that applies a policy you wrote, outside the agent's reach.

**Non-goals**
- Preventing exfiltration of what the agent can legitimately see. The network is off unless the config gives it one (see `docs/configuration.md#network`); an agent that is allowed to connect can send out what it sees, unless rules limit where.
- Non-Linux hosts.
- Defending against kernel or container-runtime escapes. Docker's isolation is assumed.

**The rule the design is built around:** the container is untrusted, the host broker is trusted. Everything the container sends (argv, env, cwd, binary name) is a *request*. The broker decides, resolves and builds everything itself. The shim in the container is a convenience, never a security boundary.

## Parts

| Part | Where | Job |
|---|---|---|
| `aicatraz` launcher | host | reads the config, starts the broker, the container, and, for `up`, the session |
| broker | host, one per session | owns the socket, applies policy, runs commands, tracks file pins, writes the audit log |
| shim | container | one small program, symlinked under the names of proxied commands, forwards the call to the broker |
| config | host, outside every mounted volume | policy and mounts; the agent can never edit it |

The broker lives and dies with the launcher process (`run`, which in a session is the program the session runs). No long-running daemon, nothing to supervise. If the broker exits, the container is stopped.

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
- The broker checks `SO_PEERCRED` and rejects peers that don't run as your user id.
- The shim reconnects with exponential backoff; broker and shim exchange heartbeats. If the shim vanishes, the broker kills the child process group. If the broker vanishes, the shim exits with a clear error instead of hanging.

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

Presets are a starting point; for commands outside the table you write the list yourself (`aicatraz config check` warns when `depends_on: auto` is used on a command without a preset, and when a workspace-executing rule has no `depends_on`).

**Honest limits**
- The pin only covers files you list (or the preset knows). A Makefile that calls `./build.sh` is covered only if `build.sh` is in the list. Keep `depends_on` generous: `scripts/**` is cheap.
- There is a small window between hashing and the executed program reading the file. The broker re-hashes immediately before `exec`, which closes the long gap but not the tiny one. For stronger guarantees, mount the pinned files `ro` (e.g. `~/git/myproject/Makefile:ro`); then the agent can't change them at all and the pin store is just a backstop.
- A file in a hidden path can **not** be a dependency: the broker would otherwise read it and put its content into a prompt. Such a rule is denied.

**The image build is pinned the same way.** The files that define it (`Dockerfile.az`, `Dockerfile.az.dockerignore`, and single files named by `COPY`/`ADD`) are checked before the launcher builds. If one is new or changed, the launcher shows the diff and asks on its own terminal (the agent isn't running yet), with `y` build once, `p` build and remember, `n` cancel. Without a terminal it refuses and says so; `--approve-build` pins non-interactively. Directories copied wholesale (`COPY . /app`) are not pinned, since that is the project's own code, which the agent legitimately edits.

### The linter

`aicatraz config check` (and every launch) warns about rules that are valid but probably not what you meant:
- **Shadowed rules.** A rule that an earlier rule already fully covers is never reached. It only reports what it is sure of: patterns are compared literally (or as `*`), so some shadowing goes unnoticed, but nothing is reported wrongly. A narrower rule in front of a broader one (the carve-out pattern) is fine.
- **`command: "*"` with `allow`**, which lets the agent run any command on the host.
- **Code runners with open arguments**: `allow` on a shell, interpreter, `env`, `xargs`, `ssh`, `find`, `sudo` and similar with no `args` (or a final `*`) is arbitrary code execution on the host. Use exact arguments or `ask`.
- **Workspace-executing commands without `depends_on`**: `allow` on `make`, `npm`, `git`, an interpreter or `docker` runs files the agent can edit.
- **Secret-looking `env_pass` names** on a rule that runs without asking.
- Plus:  `depends_on` or `env_pass` on a `local`/`deny` rule, `depends_on: auto` on a command with no preset, secret-looking `env.from_host` names.

### Flow control and slow peers

The wire protocol (version 2) never lets one stream block another. The shim may only send stdin up to a window the broker grants (256 KiB, topped up as the command consumes data), and the broker writes it to the command from a separate goroutine. So a command that doesn't read its stdin can't stop the broker from seeing a Ctrl-C, a heartbeat or a disconnect. A write to a peer that is slow but still sending heartbeats waits instead of failing; a silent peer fails it after the heartbeat timeout.

### Environment for proxied commands: whitelist, not blacklist
The broker builds the env itself:

1. A small base set from the host (`PATH` from the broker's own trusted value, `HOME`, `USER`, `LANG`, `TERM`, `TZ`).
2. Plus `env_pass` entries from the matching rule, only if the container actually sent them.
3. Minus `global.env_strip`.
4. Minus a hardcoded never-pass set the config cannot override: `LD_*`, client-supplied `PATH`, `BASH_ENV`, `ENV`, `PYTHONPATH`, `NODE_OPTIONS`, `GIT_SSH_COMMAND`, `GIT_CONFIG_*`, `PERL5LIB`, `RUBYOPT` and similar.

New variables are denied by default.

### Approval prompts

An `ask` rule, a changed pin, a build that needs approval and a network connection no rule covers all end in a prompt shown to a person on a terminal the agent can't type into or read. Three ways to show it:

- **The overlay of the built-in session** (`aicatraz up`, and the agent commands in a terminal): the session server owns the pty of the program in the jail; the client draws the prompt over the program's screen on the alternate screen, and the keys for it never reach the program. The launcher asks the session over a private unix socket that is not mounted into the jail. Keys are ignored for the first half second, so something you were typing to the agent cannot answer it. A prompt that comes while nobody is attached waits and is shown on attach.
- **A tmux popup** (`up --tmux`): a separate pty owned by the host-side tmux client.
- **A terminal you name** (`--approval-tty /dev/pts/N`): run something idle there (`sleep infinity`), not a shell that would compete for the keystrokes.

With none of them, `ask` is denied and the denial says how to fix it.

- **The prompt is hardened.** Everything in it comes from the agent, so arguments are shown quoted, with control characters, escape sequences and non-ASCII turned into visible escapes (an argument can't redraw the prompt or fake a second one); long values are cut and say so. A single key answers it; only the listed keys approve, every other key is ignored or denies. Keys typed before the prompt appeared are discarded.
- **Colors.** On a terminal the prompt is colored: file diffs in the usual way (additions green, removals red, hunk headers cyan), new files yellow and changed or deleted ones red, the answer keys highlighted, and the default (Enter) marked. Errors, notes, `config check`, `sessions`, `tools list` and the usage text are colored too. Nothing is colored when the output is not a terminal; `NO_COLOR` turns it off and `AICATRAZ_COLOR=always|never` overrides the detection. Only aicatraz's own text carries color codes: agent-controlled text is escaped first and then wrapped, so color can't be used to inject escape sequences (a test checks this).
- While waiting, the broker and shim keep exchanging heartbeats. If the agent's command is interrupted, the prompt is withdrawn. At most 4 prompts can wait at once.
- **Prompt content:** the full resolved command (real binary path, argv, cwd, passed environment variable names), the matching rule, and the file-change warning with diff when a pin differs.
- **Answers:** `y` run once, `s` run and allow this exact call (same arguments, same directory, same file contents) again for the rest of the session (memory only, never written to the config), `p` run and remember the shown file versions (only offered when files changed), anything else deny. Timeout is deny; `approval.timeout` sets it (default 2 minutes, 5 seconds to 1 hour).
- `aicatraz shell` opens another shell in the running jail (`docker exec`) from a second terminal. It uses the same broker, so host commands and prompts work there too.

### Execution details
- `subprocess` with `shell=False`, never through a shell string.
- Pty with SIGINT/SIGTERM forwarding and `SIGWINCH` resize.
- Concurrency limit per session; each command in its own process group.
- Everything runs as the host user. The broker never escalates.

## Audit log
Append-only file owned by the host user, outside every mounted volume. One JSON line per event: timestamp, session, the event (`request`, `ask`, `exit`, and later `net` for a network decision and `device` for a device attached or detached), argv, cwd, matched rule, pin status (ok / changed / new, with the files and what differs), decision and who made it, exit code, duration. Denied requests are logged too; that's where an agent probing the policy shows up.

## Order the work was done in
1. Launcher plus hardened container: `Dockerfile.az` build, same-path mounts, `rw/ro/hi` volumes, empty `~`. Already useful alone.
2. Broker and shim: token-based policy with `allow`/`deny`/`local`, whitelist env, audit log.
3. tmux integration and the `ask` flow with session grants.
4. Pin store, `depends_on`, presets, diff in the prompt; also covers `Dockerfile.az`.
5. Agent entrypoints (`aicatraz claude`, ...): tool cache, config sync, skip-permissions.
6. Canary check for hidden files, config linter, `confine_paths`.
7. Optional: built-in multiplexer, pin tooling, egress allowlist; later also sudo, published ports, devices, window titles.

## What changed from the first idea and why
- **Kept:** Docker jail, scope containment as the goal, unix-socket proxy, a wrapper that doesn't decide, allowlist with first-match-wins, `allow`/`ask` per command, same-path mounts, static and from-host container env, heartbeat and retry, pty execution.
- **Changed:** compose-style volumes; the broker builds the environment from a whitelist; the broker resolves the binary; argv is matched as tokens with flag denies; approval is drawn on a terminal the agent can't reach (first a tmux popup, now the overlay of the built-in session); the config lives outside the workspace; one broker per session.
- **Added:** per-project image from `Dockerfile.az` with the shim mounted at run time, a `local` decision so arguments choose between container and host, agent entrypoints with config sync, `hi` mode and nested overrides, dependent-file pinning with diffs, audit log format, peer-credential check, hardening flags, linter, explicit risk list.
