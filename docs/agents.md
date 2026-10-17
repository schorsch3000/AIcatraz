# Agent entrypoints

`aicatraz claude` and `aicatraz opencode` start a known agent inside the jail with no manual setup. `aicatraz agent` starts the one named by `agent.tool` in the config; In a terminal these commands run as an `aicatraz up` session (prompts over the screen, detach, status bar); `--no-session` runs the agent directly in the terminal. Arguments after `--` go to the agent.

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
- Claude Code gets `COLORTERM=truecolor`, so it draws in full color (the jail only passes `TERM` on from the host).
- opencode's database can be many gigabytes (11 GB on the machine this was built on), so it is never copied: the jail starts with an empty one per project.
- Symlinks in your config are followed (a symlinked skills directory is copied as what it points to) and nothing in the copy is a symlink. A single entry may copy at most 256 MB.
- Credentials are copied like any other synced file, so everything in the jail can read them. Settings that call host paths or commands (hooks, MCP servers) run inside the container or fail there.
- The parent directories of the mounted roots (`~/.config`, `~/.local/share`) are created owned by the user, so tools can still write next to them.

**The network.** The jail has no network unless the config gives it one, and an agent needs its service: put `network: open`, or rules that allow it (the claude example allows `*.anthropic.com` and asks about the rest), in the config. `aicatraz claude` and `opencode` say so at start when the network is off, and `config check` warns about an agent without one.

**Permission prompts.** The jail is the security boundary, so the agent's own prompts are redundant. By default the agent starts in its skip-permissions mode: `--dangerously-skip-permissions` for Claude Code (plus `--settings {"skipDangerousModePermissionPrompt":true}`, because the flag alone makes Claude ask for a confirmation on every start; the setting is passed on the command line so your own `settings.json` is not touched), `OPENCODE_PERMISSION={"*":"allow"}` for opencode. `agent.skip_permissions: false` turns it off.

**Sessions and `aicatraz resume`.** Transcripts already survive between launches. AIcatraz adds an index on top.
- Where: `~/.local/state/aicatraz/<project>/sessions.json`, not inside the mounted directories, so the agent can't edit it. It records tool, id, title, start and end time, exit code and image.
- Finding the id: for Claude Code, AIcatraz generates the id and passes `--session-id`; the session is recorded only if a transcript for it exists afterwards (an empty session can't be resumed). For opencode, which picks its own ids, the launcher asks opencode itself (`session list --format json`, in a throwaway container with the same mounts) which session in this directory was just touched.
- On exit the id is announced: `session <id> saved; continue it with aicatraz resume`.
- `aicatraz sessions` lists them, most recent first. `aicatraz resume` continues the most recent one, `aicatraz resume <id>` a specific one, `aicatraz resume --pick` shows a numbered list. It starts the jail as usual and passes the id to the agent's own option (`--resume <id>` for Claude Code, `--session <id>` for opencode).
- Ids come from files the agent can write, so they are validated (`[A-Za-z0-9_-]`, at most 100 characters) and passed as one argument, never through a shell.
- The project is identified by the `realpath` of the config file, so moving it makes the history look lost. `project.id` in the config pins the id explicitly.
