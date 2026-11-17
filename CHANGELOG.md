# Changelog

Every version notes whether it migrates stored data (chat records or config).

## Unreleased

- Copying from the browser terminal works: a mouse selection (copied by tmux via OSC 52) lands on the clipboard with a "Copied" toast, and so do the agent's own clipboard writes. Shift+drag (Option+drag on macOS) selects in the browser without tmux and copies on release or with Ctrl+Shift+C. Right click shows only the tmux menu, not the browser's as well.
- `~/.nostromo/tmux.conf` gains `set-clipboard on`. Reinstalling (and starting the server) rewrites it when it differs from the current version and reloads it into the running tmux server, so open chats pick it up without a restart.

Data migration: none.

## 0.2.0 — 2026-11-17

- **Breaking:** the launcher flags are now `--nostr-agent`, `--nostr-cwd`, `--nostr-title` and `--nostr-detach`, so they never collide with an agent's flags (Claude's `-d` is `--debug`). The old `--cwd`, `--title` and `--detach` stop with a hint; `-a` and `-d` now go to the agent.
- Every other argument of `agent` is passed to the agent's start command unchanged, e.g. `agent --nostr-detach "Fix the failing test"` starts Claude with that first message. Arguments travel through files, not the tmux command line, so any text of any length arrives byte for byte. `agent resume` does not repeat them.
- New placeholder `{args}` for `start`; a `start` without it gets the arguments appended. New per-agent `deny_args`: arguments that would break chat tracking (for Claude `--resume`, `--continue`, `--session-id`, `--print`, subcommand names, ...) are refused before anything starts.
- Fish users: values in agent commands are quoted for fish when the command runs through the login shell (backslashes used to change). The pane command now always runs under `/bin/sh` instead of tmux's `default-shell`.
- A browser on the machine itself can use `http://127.0.0.1:8787` again when `allowed_users` is set (it got `user not allowed`).
- Security: requests through `tailscale serve` with a forged `Host: localhost` are no longer treated as local; they are recognized by the proxy's headers.
- Reinstalling restarts the running service whenever its binary or unit file changed (before, a changed unit left the old server running).

Data migration: none. An existing `config.toml` keeps working: its `start` lines get the arguments appended.

## 0.1.2 — 2026-11-16

- No more "rerun agent setup": in a terminal the installer asks before running `sudo tailscale set --operator` and `sudo loginctl enable-linger`, and waits while you enable HTTPS certificates. Without a terminal it lists the steps, and the server finishes the HTTPS setup by itself within a minute after they are done (new config key `tailscale_serve`, default `true`).
- Users are never sent to `agent setup` any more: `agent` is the command that starts Claude. Repairs suggested by `agent doctor` point to the installer, which is safe to run again.
- The project is licensed under MIT.

Data migration: none.

## 0.1.1 — 2026-11-16

- The UI shows the server version under the machine name.

Data migration: none.

## 0.1.0 — 2026-11-16

First version.

- `agent` launcher: chats in tmux on a dedicated socket, `ls`, `attach`, `resume`, `stop`, `rm`.
- `agent serve`: web UI and API over `tailscale serve`, WebSocket terminal, phone layout.
- Claude Code hooks keep the resume id current and title chats from the first prompt.
- `agent setup`, `doctor`, `uninstall`, `update` with rollback; `install.sh`.

Data migration: none (chat record schema v1).
