# Changelog

Every version notes whether it migrates stored data (chat records or config).

## 0.1.3 — 2026-11-16

- A browser on the machine itself can use `http://127.0.0.1:8787` again when `allowed_users` is set (it got `user not allowed`).
- Security: requests through `tailscale serve` with a forged `Host: localhost` are no longer treated as local; they are recognized by the proxy's headers.
- Reinstalling restarts the running service whenever its binary or unit file changed (before, a changed unit left the old server running).

Data migration: none.

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
