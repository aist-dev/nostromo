# Changelog

Every version notes whether it migrates stored data (chat records or config).

## Unreleased

- Setup and doctor print the full path in "rerun setup" hints (`~/.nostromo/bin/agent setup`), because right after the first install `agent` in an open terminal can still be an old alias.
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
