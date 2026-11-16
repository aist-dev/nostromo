# Changelog

Every version notes whether it migrates stored data (chat records or config).

## Unreleased

- No more "rerun agent setup": in a terminal the installer asks before running `sudo tailscale set --operator` and `sudo loginctl enable-linger`, and waits while you enable HTTPS certificates. Without a terminal it lists the steps, and the server finishes the HTTPS setup by itself within a minute after they are done (new config key `tailscale_serve`, default `true`).
- Hints that still need setup (repairs from `agent doctor`) print its full path, `~/.nostromo/bin/agent setup`.
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
