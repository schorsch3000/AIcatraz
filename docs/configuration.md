# Configuration reference

The config is one YAML file, `aicatraz.yaml` by default. Unknown keys are errors, and `aicatraz config check` reports every problem with its line number. Everything is optional; an empty file is a valid config (an empty jail with nothing mounted).

```yaml
project:    { id: ... }
image:      ...
volumes:    [ ... ]
env:        { static: {...}, from_host: [...] }
limits:     { pids: 1024, memory: 2g }
approval:   { timeout: 2m }
canary:     { action: stop, interval: 3s }
global:     { env_strip: [...] }
rules:      [ ... ]
agent:      { tool: claude, version: latest, skip_permissions: true }
```

## `project`

| Key | Default | |
|---|---|---|
| `id` | derived | Name of the project's state directory and image. By default: the config directory's name plus a short hash of the config file's real path. Set it to keep history and pins when you move the project. Letters, digits, `.`, `_`, `-`. |

## `image`

What the container is made from.

```yaml
image: debian:13                 # a plain image reference, no build
image:                           # or a build
  dockerfile: Dockerfile.az      # default if the file exists next to the config
  context: .                     # build context, default: the config's directory
```

With neither, `Dockerfile.az` next to the config is built if it exists; otherwise `debian:stable-slim` is used. The image is built on every start (Docker's cache makes that quick) and tagged per project.

The build files (`Dockerfile.az`, the ignore files, and single files named by `COPY`/`ADD`) are pinned: a new or changed one needs your approval before the build. Every hidden path inside the build context must be excluded by `.dockerignore`, otherwise the build is refused, since `COPY . /app` would put it in the image. In a terminal, `run` and `up` offer to add each missing path to the ignore file (creating `.dockerignore` if there is none): one question per file and path, every time, and only a yes changes anything. Without a terminal, or with `--print`, nothing is asked or written and the build is refused.

The image's `ENTRYPOINT` and `CMD` are replaced. The container runs as your UID and GID whatever the image's `USER` says.

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

Rules: every path must exist at launch; `hi` entries must lie inside an `rw` or `ro` entry (nested entries are mounted on top of their parent); no duplicates; no commas in paths; you can't mount `/`, your home directory or a parent of it. Your home path exists in the jail but is an empty directory, so `~` resolves and there is nothing to find.

## `env`

```yaml
env:
  static:  { EDITOR: vim }       # set in the jail
  from_host: [LANG]              # copied from your environment at launch
```

`from_host` passes names only, so values never show up in a process list. Secret-looking names (`*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`) get a warning: the agent can read them and has network access. A `PATH` in `static` is respected; the shim directory is always put in front.

## `limits`

| Key | Default | |
|---|---|---|
| `pids` | `1024` | Maximum number of processes in the jail. |
| `memory` | none | Memory limit, e.g. `512m` or `2g`. |

The jail always drops all capabilities, sets `no-new-privileges`, and has a read-only root filesystem with tmpfs for `/tmp`, `/var/tmp`, `/run` and your (empty) home.

## `approval`

| Key | Default | |
|---|---|---|
| `timeout` | `2m` | How long a prompt waits before it counts as a deny. 5 seconds to 1 hour. |

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
- `ask`: you are asked first (popup in tmux, or `--approval-tty`); with neither, it is denied. Answers: `y` run once, `s` run and allow this exact call (same arguments, directory and file contents) for the session, `p` run and remember the shown file versions (only offered when files changed), anything else denies; timeout denies.
- `local`: the shim runs the jail's own binary. No prompt, no pins, nothing leaves the jail. This is how arguments choose between jail and host: `make build` local, `make deploy` on the host.
- `deny`: refused. Useful as a carve-out in front of a broader rule, or as an explicit last line.

The host command runs as you, in the directory the agent was in (which must lie inside a mounted volume and not in a hidden path), with a freshly built environment: a small base set (`PATH` from the broker, `HOME`, `USER`, `LANG`, `TZ`, `TERM`) plus the rule's `env_pass`. The binary is found on the broker's `PATH`; one that lives in a writable volume is never used.

**`depends_on: auto`** knows these sets: `make` (`Makefile`, `GNUmakefile`, `*.mk`, and files named by `include`), `npm`/`npx`/`pnpm`/`yarn` (`package.json`, lockfiles, `.npmrc`), `git` (`.git/config`, `.git/hooks/*`, `.gitattributes`, `.gitmodules`), script interpreters (`python*`, `bash`, `sh`, `node`, `ruby`, `perl`, `php`: the script argument), `docker` (`Dockerfile*`, compose files). For other commands list the files yourself. Pins are kept per project, outside every mount; only your `p` answer ever updates one. More than 200 dependent files can't be pinned, so such a rule asks every time.

**The linter** warns about: a rule that an earlier one already covers (never reached), `command: "*"` with `allow`, shells/interpreters/`env`/`xargs`/`ssh`/`find`/`sudo` allowed with unrestricted arguments, `allow` on `make`/`npm`/`git`/interpreters/`docker` without `depends_on`, secret-looking `env_pass` on a rule that runs without asking, `depends_on` or `env_pass` on a `local` or `deny` rule, `depends_on: auto` on a command without a built-in set.

## `agent`

| Key | Default | |
|---|---|---|
| `tool` | none | `claude` or `opencode`. Used by `aicatraz agent`, and to choose the version. |
| `version` | `latest` | `latest` (the installed current one) or a pinned version, installed on first use. |
| `skip_permissions` | `true` | Start the agent in its skip-permissions mode: `--dangerously-skip-permissions` plus `skipDangerousModePermissionPrompt` for Claude Code, `OPENCODE_PERMISSION={"*":"allow"}` for opencode. The jail is the boundary. |

Your agent settings are copied into per-project directories on every launch; the agent's own history stays there between launches. See the README and [idea-v2.md](../idea-v2.md#agent-entrypoints) for exactly what is copied.
