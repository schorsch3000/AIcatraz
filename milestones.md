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

*Status: implemented. Tested at three levels: the pin store and file resolution (unit), the broker (real socket, shim subprocess), and against real containers (the Makefile story: edit in the jail, prompt with diff, re-pin, separate projects; and the Dockerfile.az build prompt).*
- Per-project pin store (`~/.local/state/aicatraz/<project-id>/pins/`), project ID from the config's `realpath`.
- `depends_on`: presets (`auto`), globs, `{args[0]}`.
- Changed, new or missing file turns `allow` into `ask` with a warning and unified diff; `ask` rules show the same warning.
- Approve once or approve and re-pin; only human answers move pins. Re-hash right before `exec`.
- Pin results in the audit log.
- `Dockerfile.az` and the build-context files it references are pinned too: a change shows the diff and asks before `up` builds.

**Done when:** a changed `Dockerfile.az` makes `up` ask with the diff before building; editing the Makefile from inside the container turns the next `make deploy-test` into a prompt with the correct diff; re-pinning makes it silent again; two projects with an identical Makefile pin independently; the agent can't reach the pin store.

## M5: agent entrypoints

*Status: implemented. Tested with stand-in agents in real containers (config sync, persistence, discarding the agent's edits, session recording and resume for both agents), and the real installers, binaries and config locations were checked once: both agents install through the helper container, run in a Debian jail, and keep their configuration inside the mounted per-project directories. Not yet checked: a real authenticated conversation, and Claude Code's one-time "bypass permissions" confirmation in the jail.*
- `aicatraz claude`, `aicatraz opencode`: built-in profiles for install location, config files, start command.
- Tool cache on the host (`~/.local/share/aicatraz/tools/<tool>/<version>`) installed by a throwaway helper container, mounted read-only; `aicatraz tools update` and version pinning.
- Config mapping: per-project state directory mounted at the agent's config path. `sync` entries are re-copied from the real config on every launch (overwriting), `persist` entries are kept.
- Skip-permissions mode on by default, `agent.skip_permissions: false` to turn off.
- Session tracking: per-project `sessions.json` outside the mounted home; id from `--session-id` where supported, else from a before/after diff of the agent's session store, else exit-output scrape. `aicatraz sessions` and `aicatraz resume [<id>|--pick]`, with id validation. Optional `project.id` in the config.
- Resume flags confirmed against the installed versions: Claude Code `--session-id` / `--resume`, opencode `--session` and `session list --format json`.

**Done when:** after exiting `aicatraz claude`, `aicatraz resume` reopens the same conversation; the same works for opencode; `aicatraz sessions` lists both with their times; a forged or malformed id in the agent's store is rejected; moving the config with `project.id` set keeps the history; `aicatraz claude` in a fresh project starts the agent with working login and settings; editing the real settings before the next launch shows up in the jail; edits the agent made to `sync` files are gone after relaunch while its history survives; the real config directory is never mounted; the agent runs without its own permission prompts.

## M6: hardening and robustness

*Status: implemented. The canary is tested against real containers (rename over a hidden file, a replaced hidden directory, an in-place edit that must not alarm, the per-command check, switching it off). The stress tests found and fixed a real bug: a command that doesn't read its stdin, with a client that sends a lot of it, froze the broker's reader, so a disconnect or Ctrl-C was never noticed and the host process leaked. Stdin now uses credit-based flow control (protocol version 2).*
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

- **Egress allowlist.** *Status: implemented (`internal/egress`).* The design settled on IP-level filtering (an HTTP proxy was ruled out: ssh, git over ssh and database ports had to work too). A sidecar container with only `NET_ADMIN` and `NET_BIND_SERVICE` owns the network namespace and the jail joins it (`--network container:`); nftables rejects everything not allowed and redirects all TCP to a transparent proxy in the sidecar, which learns the destination (`SO_ORIGINAL_DST`), asks the host over a unix socket and passes the bytes or resets. A DNS forwarder remembers name to address, so prompts and name rules work by name; IPv6 is off. UDP and ICMP are static address rules. The host decides with the config's rules, then remembered ones, then session ones; unmatched connections ask in the `up` overlay, a tmux popup or on the approval terminal, with editable ports (`22, 80, 8000-8080`, `*`). Remembered answers live in the state directory (`aicatraz network list|allow|deny|forget`). A spike confirmed the mechanism first, and found that redirected packets still leave through the physical interface when the filter sees them, so loopback destinations are accepted by address. The default is off: without a `network` section the jail runs with `--network none`; `network: open` is the unchecked Docker network, and the claude and opencode examples ship rules that let the agent reach its service. Tested at three levels: unit tests (ports, targets, rules, DNS wire format and forwarder, proxy, control protocol, ruleset text, policy, prompt keys), the overlay through a real session, and real containers (`TestNetwork*`).
- **Built-in multiplexer replacing tmux.** *Status: implemented (`internal/mux`).* `aicatraz up` starts a detached session server that runs `aicatraz run` on a pty and serves one client at a time; `Ctrl-Y` detaches (configurable), `up` attaches again, `down` stops it. It is a pass-through multiplexer in the style of dtach, not a terminal emulator: it keeps no screen, follows only the terminal modes the program sets (alternate screen, mouse, bracketed paste, cursor, keyboard modes) so it can restore them, and makes the program repaint on attach by resizing the pty. A status bar keeps the bottom row (white on dark green by default; `--bar-theme`, `--bar-fg`, `--bar-bg`, `aicatraz bar-colors`, `--status-bar off`): the program is told the terminal has one row less and a scroll region keeps its output above the bar; the layer rewrites only the program's own scroll-region requests and repairs the bar after a screen erase, a buffer switch or a reset. A first version put the bar on the top row, which needs every absolute row address translated (and the answers back, mouse included); it worked, but a region that does not start at the top row costs the host terminal its scrollback, so it was dropped for the bottom bar. Found with opencode in kitty: erasing to the end of the screen (`CSI J`) reaches the bar row, so the bar is also repainted whenever the program has been quiet for a second; the detach key is recognised in the kitty and xterm keyboard protocols too (with num lock on, kitty sends modifier 133); leaving resets what TUIs are known to turn on (mouse, focus and sync-output modes, keyboard flags, cursor shape and color) whether or not the tracker saw it; the session starts at the cursor instead of scrolling the screen away, so the screen is as it was afterwards; `AICATRAZ_TERMLOG` records what the terminal gets and sends. The window title is `AIcatraz <project>` while attached (saved with `CSI 22;0 t`, restored with `CSI 23;0 t`), and the program's own title commands (OSC 0, 1, 2) are dropped. Approval prompts go from the launcher over a private socket to the session, which has the attached client draw them on the alternate screen; keys for the prompt never reach the agent, and keys are ignored for the first 500 ms. A prompt that comes while nobody is attached waits and is shown on attach. `up --tmux` keeps the old tmux path. `aicatraz claude`, `opencode`, `agent` and `resume` run as such a session when there is a terminal (and no `--approval-tty`), so their approval prompts just work; `--no-session` runs them in the terminal as before. Tested with a stand-in program and a pty-driven client (modes, detach and reattach, prompts, takeover, withdrawal), and against real containers (`TestUpSession`).
- **Devices.** *Status: implemented (`internal/devices`).* `devices:` names hardware by by-id name, USB vendor:product (and serial number) or path. The jail starts with device-cgroup rules for the classes (ttyACM 166, ttyUSB 188, raw USB 189) and for the exact numbers of path matches; the launcher polls the host (sysfs and /dev, every 100 ms at first, then 300 ms) and makes and removes nodes in the running jail with `docker exec --privileged` of our own binary (`__dev`), owned by the jail user, with the by-id links. A spike showed the jail user can open such a node, a node outside the rules can be made but not opened, and it works under no-new-privileges and a read-only root. Real sysfs showed that `bus/usb/devices` entries are links, so the walk starts at the real directory. `aicatraz devices` lists the USB devices. Tested with a fake host (sysfs and /dev in a temp directory) and a fake jail for plug, unplug, re-plug under another name and a reset, and against real containers with `/dev/fuse` and a real serial adapter (looked at, never opened).
- **Published ports.** *Status: implemented.* `ports:` publishes ports of the jail on the host (`3000`, `8080:80`, `address:host:container[/udp]`, ranges), on 127.0.0.1 unless an address is written out; the jail publishes them with `network: open`, the sidecar with rules, and `network: off` makes it a config error. Tested with a server in the jail reached from the host in both modes.
- **Pin store tooling.** *Status: implemented.* `aicatraz pins list` shows every pin with its state against the file now (`ok`, `changed`, `missing`), short hash, size and approval time; `aicatraz pins forget <path>... | --missing | --all` removes pins (all targets are checked before anything is removed); `aicatraz pins diff [<path>...]` shows what changed in pinned files (contents escaped, large and binary files said so). Covered by unit tests and an extra step in the Makefile integration test.

## Order and risk
M0 to M2 carry the real risk (protocol, pty handling, policy correctness). Do those first and keep them small. M1 is worth shipping early because it already delivers the hide, read-only and containment value without the broker. M3 and M4 are the features that make daily use pleasant and safe, and each depends only on M2. M5 depends only on M1 (image, mounts), so it can be pulled forward if you want to use the jail with a real agent before the broker exists; the agent just won't have a door to the host yet.
