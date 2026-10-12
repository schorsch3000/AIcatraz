# AIcatraz

A Docker-based jail for AI agents so they can't break out onto your machine in ways you don't like.

# core concept

AIcatraz jails your AI Agent (or any executable by that matter) in a Docker container while allowing controlled command execution on the host.

You give it:
- Project workspace or configuration as a read/write volume
- Installed tools, documentation, source code as a read-only volume
- Permissions to run specific commands on your local machine via a proxy

Example: `make deploy` can run from inside the container, but the agent can't see deployment secrets (SSH keys) or SSH into production directly.

# architecture

## containment
- Docker container with dropped capabilities (`--cap-drop=all`)
- Agent has full freedom inside the container — no restrictions on `exec`, `sudo`, or anything else within the container
- The threat model is **scope containment**: preventing operation on files outside mounted volumes, not stopping all data exfiltration

## command execution: 3-stage proxy
Commands that should run on the host (deploy scripts, etc.) go through a proxy system:

1. **Wrapper in container** — intercepts command invocation, forwards to Unix socket
2. **Unix socket** — transports request (env, argv, stdin) from container to host
3. **Proxy server (outside container)** — checks allow list, executes command on the real filesystem with restricted env

### path mapping
The project directory is mounted at the same absolute path on both host and container. This makes `$PWD` and `~` (home directory) identical in both environments, eliminating path translation complexity.

### env vars

there are t stages to consider:
1. **Container env** — the configfile can set static or dynamic env vars for the container. The agent sees these.
    - static are configured my name and value
    - dynamic are configured by name only, the value is taken from the host env
2. **Proxy command env** the procy command tries to replicate the environmend ad good as possible, meaning reading args, mapping stdin, stdout, stderr and environement variables. but there can be variables that are blacklisted by name for any given command.

# command proxy details

## wrapper binary
A single script replaces commands that need host execution. It:
- Reports its invocation to a Unix socket outside the container
- Forwards env vars, argv, and stdin to the proxy server
- Receives stdout/stderr and exit code from the proxy server
- Does NOT decide what to proxy — the proxy server on the host checks the allowlist

## proxy server
- Listens on a Unix socket owned by the user who started the sandbox
- The container user has the same UID/GID as the host user
- Each command is executed as a separate fork via `subprocess.Popen`
- Commands are run with an attached pty for interactive terminal access
- If the command requires user confirmation (`prompt: true`), the proxy prompts in the real terminal
- If `prompt: false`, it runs immediately
- Command-level granularity: `make *` can be prompted while `make deploy-test` can be unprompted
- Executes the **real binary on the host** (passed from the wrapper) — not inside the container

## client (inside container)
- Connects to the Unix socket
- Waits for responses, receives heartbeats
- Retries on connection failure with exponential backoff

# sudoers-style config
Configuration manages access granularly:

### Volume config
- Single `path` field — host and container use the same absolute path
- `mode: rw` or `mode: ro`

### Env config
- `env.static` — key=value pairs set inside the container
- `env.from_host` — list of variable names to pass through from the host environment
- `strip_env` (global) — env vars stripped from ALL proxy command executions

### Command config
- `external-commands` — allowlist only (no deny list). If a command is not listed, it cannot be executed on the host.
- Each rule: `command` (supports glob patterns like `make *`), `prompt: true|false`, `strip_env` (additional per-command vars)
- **Match order: first matching rule wins (strict top-down)**
- Per-command `strip_env` is IN ADDITION to the global `strip_env`

# security considerations
- The agent can run `exec`/`sudo` inside the container — this is acceptable since containment is enforced by Docker volumes and capabilities
- If `exec`/`sudo` is passed to the proxy wrapper, the external allow list catches it (or allows it if explicitly permitted)
- Symlink traversal to files outside volumes is documented as an accepted risk
- The proxy process is a single point of failure — should be supervised and log all activity
- Heartbeat and retry logic prevents the agent from hanging if the proxy crashes
- Outbound network is allowed (agent needs API access) — scope containment is the primary goal, not network blocking

