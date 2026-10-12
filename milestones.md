# AIcatraz milestones

Stack: Go, one static binary that dispatches on `argv[0]` (launcher, broker, shim, approval prompt), YAML config, `docker` and `tmux` driven through their CLIs. See `idea-v2.md` for the design.

Each milestone ends in something usable and has a "done when" list that can be checked by a test, not by feel.

## Running thread: adversarial tests

From M1 on, a test suite plays the hostile agent. Every milestone adds cases for what it introduced, and the suite runs in CI against a real Docker daemon. Examples: read a hidden file, write a `ro` path, call the socket with a forged binary path, send `LD_PRELOAD`, symlink out of the workspace. A milestone isn't done until its attack cases pass.

## M0: skeleton and config
Repo, Go module, `argv[0]` dispatch, CI (build, vet, unit tests), strict YAML config loader with clear errors.
- `aicatraz config check <file>` validates a config and prints problems with line numbers.
- Unknown keys, bad modes, nested volume entries outside a parent, and `~` expansion are all handled.

**Done when:** valid and invalid example configs behave as specified in unit tests; the binary builds statically.

## M1: the jail (useful on its own)

*Status: implemented as `aicatraz run [-c file] [--print] [-- command...]`; `make test-integration` runs the checks below against real containers. The shim directory is built and mounted but `PATH` is not changed yet, because the shim is still a stub and would shadow the container's own `make`; the `PATH` prepend lands with M2.*
Launcher turns a config into a hardened `docker run`.
- Image from `Dockerfile.az` (or a plain image reference, or a built-in default), built on every `up` and tagged per project. Image `ENTRYPOINT`/`CMD` replaced by AIcatraz's init; `--user` with the host UID and generated read-only `/etc/passwd` and `group`.
- Launcher refuses to build if a `hi` path is not excluded by `.dockerignore`.
- Shim directory (symlinks plus binary) mounted read-only and prepended to `PATH`; built here, used from M2.
- Same-path mounts, `rw`/`ro`/`hi` with nested overrides (`/dev/null` over files, empty tmpfs over directories).
- Empty tmpfs `~` at the real home path, same UID/GID, `--cap-drop=all`, `no-new-privileges`, read-only root, tmpfs `/tmp`, pids and memory limits.
- `env.static`, `env.from_host`, secret-name warnings.
- Hide target must exist at launch; `aicatraz run --print` shows the exact `docker run` line.

**Done when:** inside the container, hidden files read as empty and can't be deleted, `ro` paths reject writes, `~` is empty but `$HOME` is the host path, `capsh --print` shows nothing, `sudo` gains nothing. Works on a Debian base and on at least one image with a different `USER` (and an Alpine one for the static shim binary); a `COPY . /app` Dockerfile with an unexcluded `.env` fails the build step.

## M2: broker and shim, `allow`/`deny`

*Status: implemented. The broker runs inside `aicatraz run` for the length of the session; `ask` rules are denied with an explanatory message until M3. Covered by broker tests (real socket, shim as a subprocess, pty sessions, signals, group kill) and by Docker integration tests.*
The first version of the door.
- Unix socket (only the socket file mounted), `SO_PEERCRED` check, versioned protocol, heartbeat, reconnect with backoff.
- Shim forwards name, argv, cwd, stdin, env request; broker resolves the binary itself.
- Token-based rule matching with `deny_flags`, first match wins, default deny.
- Decisions `allow`, `deny` and `local` (broker answers, shim `exec`s the container's own binary found past `/aicatraz/bin`).
- Whitelist environment with the hardcoded never-pass set.
- pty execution, signal forwarding, resize, process-group cleanup on disconnect, concurrency cap.
- Append-only audit log, denied requests included.

**Done when:** `make deploy-test` runs on the host and its output and exit code come back; `make build` with a `local` rule runs inside the container and never reaches the host; `make -f x deploy-test` is denied; a forged binary path is ignored; `LD_PRELOAD` and `PATH` from the container never reach the command; Ctrl-C works; killing the broker makes the shim exit with an error within the heartbeat timeout; killing the shim kills the child.

## M3: `ask` and the tmux session

*Status: implemented. Prompts, answers, session grants, timeouts, heartbeats during the wait, withdrawal on interrupt, the `--approval-tty` fallback and `shell` are tested against real containers. The tmux layer (`up`, popup) is tested against a stand-in `tmux` script only, because tmux is not installed on this machine; it needs one run against real tmux. The broker lives inside `aicatraz run` rather than in a pane of its own.*
- `aicatraz up` creates the tmux session (agent pane plus broker), `aicatraz shell` attaches.
- `ask` shows a `tmux display-popup` with the full resolved command, cwd, env differences and matched rule.
- Answers: once, for this session (exact argv, memory only), deny. Timeout means deny. Fallbacks: approval pane and `--approval-tty`.
- Broker exit stops the container; clean teardown.

**Done when:** a scripted run answers prompts through tmux; the agent's own input can't reach or answer a prompt; no answer within the timeout is a deny; the log records who decided.

## M4: pins and dependent files
- Per-project pin store (`~/.local/state/aicatraz/<project-id>/pins/`), project ID from the config's `realpath`.
- `depends_on`: presets (`auto`), globs, `{args[0]}`.
- Changed, new or missing file turns `allow` into `ask` with a warning and unified diff; `ask` rules show the same warning.
- Approve once or approve and re-pin; only human answers move pins. Re-hash right before `exec`.
- Pin results in the audit log.
- `Dockerfile.az` and the build-context files it references are pinned too: a change shows the diff and asks before `up` builds.

**Done when:** a changed `Dockerfile.az` makes `up` ask with the diff before building; editing the Makefile from inside the container turns the next `make deploy-test` into a prompt with the correct diff; re-pinning makes it silent again; two projects with an identical Makefile pin independently; the agent can't reach the pin store.

## M5: agent entrypoints
- `aicatraz claude`, `aicatraz opencode`: built-in profiles for install location, config files, start command.
- Tool cache on the host (`~/.local/share/aicatraz/tools/<tool>/<version>`) installed by a throwaway helper container, mounted read-only; `aicatraz tools update` and version pinning.
- Config mapping: per-project state directory mounted at the agent's config path. `sync` entries are re-copied from the real config on every launch (overwriting), `persist` entries are kept.
- Skip-permissions mode on by default, `agent.skip_permissions: false` to turn off.
- Session tracking: per-project `sessions.json` outside the mounted home; id from `--session-id` where supported, else from a before/after diff of the agent's session store, else exit-output scrape. `aicatraz sessions` and `aicatraz resume [<id>|--pick]`, with id validation. Optional `project.id` in the config.
- Confirm the real resume flags of the installed Claude Code and opencode versions before wiring them up.

**Done when:** after exiting `aicatraz claude`, `aicatraz resume` reopens the same conversation; the same works for opencode; `aicatraz sessions` lists both with their times; a forged or malformed id in the agent's store is rejected; moving the config with `project.id` set keeps the history; `aicatraz claude` in a fresh project starts the agent with working login and settings; editing the real settings before the next launch shows up in the jail; edits the agent made to `sync` files are gone after relaunch while its history survives; the real config directory is never mounted; the agent runs without its own permission prompts.

## M6: hardening and robustness
- Canary check for `hi` files (inode seen from inside the container, per command and on a timer), session frozen with a warning if one is exposed.
- Config linter: shadowed rules, bare `*` with `allow`, workspace-executing commands without `depends_on`.
- `confine_paths`, `realpath` checks on cwd.
- Stress cases: many concurrent commands, huge stdin and stdout, abrupt disconnects, broker restart.

**Done when:** the host-side "rename over `.env`" scenario is detected; the linter catches the known bad configs; the adversarial suite is green; no leaked processes or sockets after any teardown path.

## M7: release polish
- `aicatraz init` writes a sample config and the symlink list for the image.
- Example configs for common setups, user docs (threat model and known risks included), reference Dockerfile.
- Versioned release builds, reproducible build notes.

**Done when:** someone else can go from clone to a working jail with the README alone.

## M8: optional, later
- Egress allowlist (domain-filtering proxy in front of the container).
- Built-in multiplexer replacing tmux.
- Pin store tooling (`aicatraz pins list|forget`).

## Order and risk
M0 to M2 carry the real risk (protocol, pty handling, policy correctness). Do those first and keep them small. M1 is worth shipping early because it already delivers the hide, read-only and containment value without the broker. M3 and M4 are the features that make daily use pleasant and safe, and each depends only on M2. M5 depends only on M1 (image, mounts), so it can be pulled forward if you want to use the jail with a real agent before the broker exists; the agent just won't have a door to the host yet.
