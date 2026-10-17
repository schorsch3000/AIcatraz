# Security: threat model and known risks

## What this is for

An AI coding agent is useful because it can run commands and edit files. It is risky for the same reason. AIcatraz limits the damage a confused or misled agent can do to **the scope you mount**, and puts a policy you wrote in front of the few things that must happen on your machine.

**The goal is scope containment:** the agent can only operate on the files you mounted, and can only run on the host what the rules allow, with the arguments they allow, with the files you approved.

## What is not a goal

- **Stopping exfiltration of what the agent can legitimately see.** The jail has no network unless you give it one. An agent has to be given its service (`network: open`, or rules), and then anything you mount (and anything you pass in `env.from_host`, and your agent credentials, which are copied in) can be sent out. Don't mount what must stay secret; hide it (`hi`) or keep it out.
- **Defending against escapes from Docker or the kernel.** The jail is as strong as Docker's isolation with all capabilities dropped, `no-new-privileges`, a read-only root, and the pid and memory limits you set. It is not a hardened sandbox against a determined kernel exploit.
- **Protecting a machine where the Docker socket is already reachable by the agent.** It never is mounted, but anyone who can use Docker as you can do anything as you.
- Non-Linux hosts.

## Trust boundaries

| | Trusted? |
|---|---|
| The container and everything in it (the agent, the shim, files in rw volumes) | **No.** |
| The broker, the launcher, the config, the pin store, the audit log (all outside every mount) | Yes. |
| You, at the approval prompt | Yes. |

Everything the container sends is a *request*: command name, arguments, working directory, environment, even the shim itself. The broker validates and decides everything, and resolves the binary itself.

## What is enforced

- **Mounts.** Only listed paths exist. `hi` paths read as empty and can't be removed. Your home is an empty directory at the real path.
- **The door.** Default deny. Arguments are matched token by token. Rules never grant a call that uses one of their `deny_flags`. The broker only accepts connections from your UID, on a socket in a private directory that is mounted read-only into the jail.
- **The environment** of a host command is built from scratch, never forwarded. A fixed set of dangerous variables is never passed whatever the config says.
- **The binary** is looked up by the broker on its own `PATH`; one that lives in a writable volume is skipped, so the agent can't plant a `make` and have it run on the host.
- **The working directory** must be inside a mounted volume and not inside a hidden path.
- **Pinned files.** A rule can depend on files (`depends_on`); a change turns `allow` into `ask` with a diff. Only a human `p` answer moves a pin, the pin is the snapshot shown, and the files are re-checked just before the command starts. The build files of the image are pinned the same way.
- **The approval prompt.** It is shown on a terminal the agent can't type into or read: the overlay of the built-in session (`aicatraz up`), a tmux popup (`up --tmux`), or a terminal you name (`--approval-tty`). Everything agent-controlled in it is escaped, so it can't redraw the prompt or fake a second one. Only `y`, `s` and `p` approve; type-ahead is discarded; timeout is deny; at most 4 prompts wait at once. In the built-in session the prompt is drawn by the client on the alternate screen, the terminal is put back to its plain modes while it is up (so mouse and focus reports can't arrive as an Esc), keys are ignored for the first half second so a key you were typing to the agent can't answer it, and none of the keys you press for the prompt go to the agent. The requests travel over a socket in a private directory that is not mounted into the container. What the agent can still do is print text that *looks* like a prompt in its own pane; that grants nothing, because only the overlay's keys count.
- **Hidden paths stay hidden**, or the session ends: a canary checks from inside the container, on a timer and before every host command.
- **The audit log** records every request, decision, who decided and the exit status. If it can't be written, the command doesn't run.
- **Resource safety.** A command that doesn't read its stdin can't freeze the broker; a disconnect kills the command's whole process group; a limit on concurrent host commands.

## Known risks, stated plainly

1. **Allowed commands that run workspace content.** `allow` on `make`, `npm run`, a script, `git` (hooks, aliases), `ssh` (`ProxyCommand`) is host code execution *with that rule's authority*: the agent writes the Makefile, the host runs it. Mitigations: `depends_on` (pins, with diffs), exact `args`, `ask`, and mounting the pinned files `ro`. The pin covers only the files you list or the preset knows; a Makefile that calls `./build.sh` is covered only if `build.sh` is too. The linter flags the common cases.
2. **Planted files that fire later.** The agent can write `.git/hooks/*`, `.envrc`, `.vscode/tasks.json`, shell rc files in the project. They run when *you* later use the project outside the jail. Mount the dangerous ones `ro` (for example `.git/hooks`, `.git/config`), and review diffs before you run anything.
3. **Check-then-run windows.** The broker checks the working directory, then starts the command; it re-checks pinned files just before. In those instants the agent could swap a directory or file. The command still has to be one the policy allows, and it runs as you in a different place, so the impact is limited but not zero. `ro` or `hi` mounts remove the option.
4. **Credentials are copied in.** For agents, your credentials file is copied into the jail's state on every launch, so everything in the jail can read it. Hooks and MCP servers in your agent settings run inside the jail.
5. **Hidden files are only as safe as the mount.** A hidden file is `/dev/null` bind-mounted over it. If the host replaces it by rename, it is exposed; the canary notices (within its interval, or at the next host command) and stops the session by default, but in that window the agent could have read it. Hide a directory instead of a single file.
6. **Symlinks in rw volumes** are harmless inside the jail (it can't resolve them outside its mounts), but can mislead host-side tools. `confine_paths` resolves arguments to real paths, and the working directory is always resolved.
7. **Network is off by default.** `network: open` gives the jail Docker's normal network, unchecked; rules limit it as described below, and whatever the rules cannot see (see their limits) stays open.
8. **The broker is one process per session.** If it dies, the shim fails clearly instead of hanging, and nothing runs on the host. A broker that was killed hard leaves a socket directory behind, cleaned up on the next start.
9. **`local` is not a boundary.** It runs the jail's binary in the jail; it only decides *where*, never what the agent may do inside.

## Sudo

`sudo: true` is a deliberate hole in the hardening, off by default. It gives the agent root inside the container, so the agent can install packages, change anything in the container, and read and write every file on your read-write volumes regardless of its permissions. In exchange:

- **What stays enforced for root:** hidden paths still read as empty and can't be unmounted or deleted, read-only volumes and the generated `/etc/passwd` and `/etc/group` stay read-only, root has no `SYS_ADMIN`, `NET_ADMIN`, `NET_RAW`, `SYS_PTRACE`, `MKNOD` or `DAC_READ_SEARCH`, and the broker still only accepts your UID, so root can't speak for you at the door.
- **What is lost:** `no-new-privileges` and the read-only root filesystem (the container is removed on exit, so the writable layer doesn't survive), and the capability set is no longer empty. Anything that relies on the agent being unable to become root inside the container, such as a rule that trusts files the agent can only write with root rights, no longer holds.
- **Files created as root** in a read-write volume stay owned by root on your machine (`sudo npm install` in the project, for example). You may need to `sudo chown` them back.
- It is still a container: an escape needs a kernel or Docker bug, as before, but the extra capabilities widen the surface somewhat.

## Network rules

With a `network` section, a helper container (the sidecar) owns the jail's network namespace and the jail joins it. The jail keeps `--cap-drop=ALL`, so it has neither `NET_ADMIN` (which changes firewall rules) nor `NET_RAW`, and `sudo: true` doesn't add them. The sidecar has `NET_ADMIN` and `NET_BIND_SERVICE` and nothing else, runs our own binary, and talks to the host over a socket in the private run directory that is not mounted into the jail.

- **What is enforced:** the firewall rejects everything the rules do not allow, over every protocol, IPv6 included (switched off). Every TCP connection goes through a proxy that asks the host; UDP and ICMP are allowed only by an address rule. The proxy and the DNS forwarder carry a socket mark that the firewall lets out, and the jail can't set one.
- **Private and special addresses** can be allowed only by an IP or CIDR rule, never by a host name or `*`; a name that resolves to one (DNS rebinding) stays refused.
- **Fail closed:** if the host process dies the sidecar exits, and the firewall stays in the jail's namespace with nothing listening, so connections fail.
- **Limits:**
  - The decision is by destination address and port. A rule for a name allows the addresses the jail's own lookup of that name returned; a CDN address shared with other sites is allowed for all of them.
  - TLS isn't inspected, so the allowed destination is trusted with whatever is sent to it. An allowed host that relays (a proxy, a paste site, a git host the agent can push to) is a way out.
  - DNS: with `dns: open` any name resolves and an attacker-chosen name can carry data in the query; `dns: restricted` closes that.
  - The jail sees a TCP connection as open until the first read or write, because the proxy accepts it before the decision falls.
  - Host commands you allow through `rules` run on the host's network, outside all of this.
  - It is only as strong as Docker's isolation: a kernel bug is still a way out.

## Published ports

`ports:` opens a way *into* the jail. By default a port is published on 127.0.0.1 only, so other machines can't connect, but everything on this machine can: every local user, every program, and every web page in your browser (a page can send requests to `127.0.0.1`). A server the agent starts on a published port answers all of them. Publish only what you need, keep the address narrow (`0.0.0.0` makes it reachable from the network, and `config check` warns about it), and treat a published port as an interface the agent can use to reach your browser. Incoming connections are not checked by the `network` rules; those control what the jail connects to.

## Reporting a problem

This is early software with a test suite that includes an adversarial part (forged binary paths, escape sequences in arguments, symlink and `..` tricks, changed files during approval, exposed hidden paths). If you find a way around a guarantee above, please report it with the exact config and commands.
