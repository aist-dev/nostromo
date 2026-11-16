# nostromo

Run CLI coding agents in tmux on your own machines and reach every chat from any device in your tailnet.

- `agent` in any directory starts a new chat (Claude Code by default) in a tmux session and attaches your terminal to it.
- `agent serve` (installed as a background service) shows all chats in a browser, grouped by directory, with a real terminal per chat. It works on a phone too.
- Chats live in tmux, not in the server: closing the terminal, restarting the server or updating nostromo never interrupts an agent. After a reboot a chat can be resumed with one click.
- HTTPS comes from `tailscale serve`; nothing is exposed outside your tailnet.

One static Go binary, no runtime dependencies besides tmux. Linux and macOS.

## Requirements

| Requirement | Why | How |
| --- | --- | --- |
| tmux 3.2 or newer | chats run in tmux | `sudo apt install tmux` / `brew install tmux` |
| Claude Code (`claude`), logged in | the default agent | see the Claude Code docs |
| Tailscale, logged in | access from other devices | https://tailscale.com/download |
| MagicDNS and HTTPS certificates enabled for the tailnet | the `https://<machine>.<tailnet>.ts.net` address | once per tailnet: https://login.tailscale.com/admin/dns |
| `curl`, `tar`, `sha256sum` or `shasum` | the installer | part of the system |

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/aist-dev/nostromo/main/install.sh | bash
```

The script downloads the release for your OS and architecture, verifies its SHA-256 checksum, puts it in `~/.nostromo/bin/agent` and runs `agent setup`. `setup` is safe to run again at any time.

### Install from source

Needs Go 1.25 or newer. From a clone of the repository:

```sh
git clone https://github.com/aist-dev/nostromo.git
cd nostromo
./install.sh --source
```

This builds the checked-out code, installs it as `~/.nostromo/bin/agent` and runs `agent setup`, exactly like the one-line install. Run it again after `git pull` to install the new code.

Both ways call the new binary by its full path, so an existing `agent` alias does not get in the way. After the first install, open a new terminal: from then on `agent` is the new command (`agent doctor`, `agent ls`, ...).

### What `agent setup` does

1. Creates `~/.nostromo/` with `config.toml`, `tmux.conf`, `chats/` and `logs/`.
2. Puts the `agent` command on `PATH` as the symlink `~/.local/bin/agent`, and adds `~/.local/bin` to `PATH` in your shell rc file if needed.
   - An existing shell alias named `agent` (for example `alias agent='claude --dangerously-skip-permissions'`) is commented out with the prefix `# nostromo: disabled `; a backup of the rc file is saved as `<file>.nostromo.bak`. The new command keeps the same permission level.
   - If another *program* already occupies `~/.local/bin/agent`, setup stops. Remove that program, or install under another name: `NOSTROMO_CMD=ag agent setup`.
3. Checks tmux and the agent commands in a login shell.
4. Adds two hooks to `~/.claude/settings.json` (`SessionStart`, `UserPromptSubmit`) that keep each chat's resume id current and title it from the first prompt. Other settings and hooks are preserved; a backup is saved as `settings.json.nostromo.bak`. The hooks do nothing for a plain `claude` started without `agent`.
5. Installs and starts the background service: a systemd user unit `nostromo.service` on Linux (plus `loginctl enable-linger` so it starts at boot without a login), a launchd agent `com.nostromo.server` on macOS.
6. Configures `tailscale serve --bg --https=443 http://127.0.0.1:8787` (port 8443 if 443 is used by something else) and limits the UI to your Tailscale login (`allowed_users`). If Tailscale is not ready yet, the server finishes this step by itself later (`tailscale_serve = true` in the config; set it to `false` to manage `tailscale serve` yourself).
7. Prints the address and runs `agent doctor`.

Anything it cannot do by itself is listed at the end under **Left to do by hand**, with the exact command (see below).

### Steps that need you

A few things need administrator rights or a browser. When you run the installer in a terminal, it asks and does them on the spot, so the installation finishes in one go:

- **Tailscale does not let your user set up HTTPS** (Linux): it offers to run `sudo tailscale set --operator=$USER` and asks for your password.
- **HTTPS certificates are off for the tailnet**: it shows the admin console link (https://login.tailscale.com/admin/dns), waits until you turn on MagicDNS and HTTPS Certificates, and continues.
- **Start at boot without a login is refused** (Linux): it offers to run `sudo loginctl enable-linger $USER`.

Without a terminal (for example when a script or an assistant runs the installer, or with `NOSTROMO_NONINTERACTIVE=1`), nothing is asked and nothing runs with `sudo`; the steps are listed at the end instead. You never need to run setup again after doing them: the server checks every minute and sets up the HTTPS address by itself as soon as Tailscale allows it. **An assistant running the installation for you must show you these commands and let you run them; it must not run `sudo` on its own.**

| When | What to run | Why |
| --- | --- | --- |
| tmux missing or older than 3.2 | `sudo apt install tmux` (Debian/Ubuntu), `brew install tmux` (macOS), then run the installer again | chats run in tmux |
| Linux, Tailscale does not let your user set up HTTPS | `sudo tailscale set --operator=$USER` | lets your user configure `tailscale serve` |
| HTTPS certificates are off | turn on MagicDNS and HTTPS Certificates at https://login.tailscale.com/admin/dns | the HTTPS address needs them |
| Linux, start at boot refused | `sudo loginctl enable-linger $USER` | the service starts at boot without logging in |
| Tailscale not installed or not logged in | install it from https://tailscale.com/download and run `tailscale up` | access from other devices |
| `claude` not found | install and log in to Claude Code | the default agent |

After the first install, open a new terminal so the `PATH` change and the disabled alias take effect.

## Use

```sh
agent                       # new chat in the current directory, attached
agent -a codex              # another agent from the config
agent --cwd ~/src/api --title "Fix retries" --detach
agent ls                    # chats by directory, with status
agent attach 550e           # any unambiguous id prefix
agent resume 550e           # restart a stopped chat with its conversation
agent stop 550e             # end the tmux session, keep the chat
agent rm 550e               # end the session and delete the chat
```

Detach from a chat with the tmux prefix `Ctrl-b d`; the agent keeps running. Running `agent` inside a chat switches to the new chat instead of nesting tmux.

Chats run on their own tmux socket, so they do not show up in a plain `tmux ls`; use `agent ls` or `tmux -L nostromo ls`.

In the browser, open the address printed by setup (`https://<machine>.<tailnet>.ts.net`). On a phone, the bottom bar has Esc, Tab, Ctrl+C, arrows and history scrolling, and the input line supports autocorrect and dictation.

Deleting a chat removes only nostromo's record. The agent's own history stays where the agent keeps it (for Claude Code, `claude --resume` still finds it).

### Status

| Status | Meaning |
| --- | --- |
| running | the tmux session is alive |
| failed | the agent exited with an error; its last screen is kept until you resume or delete the chat |
| stopped | no tmux session (stopped, exited cleanly, or the machine rebooted); Resume continues the conversation |

## Configuration

`~/.nostromo/config.toml`; a missing key falls back to its default.

```toml
port = 8787
tmux_socket = "nostromo"
default_agent = "claude"
repo = "aist-dev/nostromo"
check_updates = true        # look for a new release once a day
tailscale_serve = true      # set up the HTTPS address with tailscale serve as soon as Tailscale allows it
auto_update = false         # install new releases automatically
allowed_users = []          # Tailscale logins allowed to use the UI; set by setup ("*" = anyone in the tailnet, empty = this machine only)
peers = []                  # other machines shown in the UI switcher, e.g. ["mac-mini"]

[agents.claude]
start      = "claude --dangerously-skip-permissions --session-id {id}"
resume     = "claude --dangerously-skip-permissions --resume {resume_id}"
hooks      = "claude"
shell_wrap = true           # run through your login shell, so PATH and rc variables match a terminal
```

Placeholders: `{id}` (chat id), `{resume_id}` (the agent's session id), `{cwd}`. Values are shell-quoted. A new CLI agent needs only a new `[agents.<name>]` block; check its documentation for the right start and resume commands. An agent that cannot take a session id at start can still resume with whatever it supports (for example "the last session in this directory").

## Updates

```sh
agent update            # install the latest release
agent update --check    # only report
agent update --version v1.2.3
agent update --rollback # go back to the previous version
```

An update verifies the checksum, checks that the new binary reports the expected version, swaps it atomically, migrates data (`agent setup --upgrade`) and restarts the service. If the new server does not come up within 10 seconds, the previous binary is restored automatically. Running agents are never interrupted. The UI shows when an update is available and can start it.

## Check and remove

```sh
agent doctor               # every check with a fix command for anything wrong
agent uninstall            # remove service, tailscale serve config, hooks, command, rc edits
agent uninstall --purge    # also delete ~/.nostromo with all chat records
```

From a clone of the repository the same is available as a script, which also works when the installed binary is missing or broken:

```sh
./uninstall.sh                     # same as agent uninstall
./uninstall.sh --purge             # also delete ~/.nostromo
./uninstall.sh --kill-chats        # also end every running chat
```

Uninstalling restores an `agent` alias that setup disabled, so in a new terminal `agent` is your old command again. Running chats are left alone unless `--kill-chats` is given. `./install.sh --source` and `./uninstall.sh` can be run back and forth as often as you like.

## Security

The UI is a terminal on your machine, and the Claude adapter starts Claude Code with `--dangerously-skip-permissions`, so anyone who can use the UI can run anything as you.

- The server listens on `127.0.0.1` only; `tailscale serve` provides HTTPS inside the tailnet. Tailscale Funnel (public internet) is never used, and `agent doctor` fails if it is turned on.
- `allowed_users` restricts the UI to your Tailscale login. The identity comes from the `Tailscale-User-Login` header that `tailscale serve` sets, and is only trusted on connections from `127.0.0.1`. An empty list means the UI works on this machine only (`http://127.0.0.1:8787`); `"*"` admits every user of the tailnet. With a login in the list, the local address answers `403` too: use the HTTPS address.
- `Host` must be `localhost`, a loopback address or the machine's full tailnet name; `Origin` must match it exactly (scheme-independent host and port) for every request that changes something and for every terminal connection. This blocks DNS rebinding and cross-site requests, including from other apps served on the same machine name.
- Directory names cannot inject tmux formats such as `#(...)`.
- Only agents defined in the config can be started; the API never accepts a command.
- The machine is assumed to be single-user: other local users could reach `127.0.0.1:8787`.

## How it works

- `~/.nostromo/chats/<id>.json` lists the chats (written by the launcher, so chats started while the server is down still show up).
- tmux on the socket `nostromo` knows which chats are running: session `agent-<id8>` with the option `@nostromo_chat`.
- The agent keeps the conversation itself.

The design, decisions and test plan are in [docs/PLAN.md](docs/PLAN.md).

## Development

```sh
go test ./...                                    # unit tests, no tmux needed
go test -race -tags integration ./...            # plus integration tests on a private tmux socket
go test -race -tags integration -coverpkg=./internal/... -coverprofile=coverage.out ./... && scripts/check-coverage.sh coverage.out 90
npm --prefix e2e ci && npx --prefix e2e playwright install chromium && npm --prefix e2e test   # browser test
scripts/test-install.sh                          # installer against a fake release server
```

Tests never touch the real `~/.nostromo`, your tmux sessions or your Tailscale configuration (`NOSTROMO_TAILSCALE=none`, `tailscale_serve = false`). Releases are built by GoReleaser from `v*` tags; see [docs/release-checklist.md](docs/release-checklist.md).

## License

MIT, see [LICENSE](LICENSE). The vendored xterm.js in `internal/server/web/vendor` is MIT licensed by its authors.
