# nostromo — tmux launcher and server for CLI agents

This is the design document and implementation plan. When code and plan disagree, update the plan first.

## 1. Overview

A single static Go binary named `agent`: launcher, server, installer and updater in one. The project, repository, data directory (`~/.nostromo`), service and environment-variable prefix are all named `nostromo`. The user-facing command is `agent`.

Three sources of truth, each with its own role:

| What | Where it lives | Who writes it |
| --- | --- | --- |
| Which chats exist | `~/.nostromo/chats/<id>.json`, one file per chat | `agent` on launch; the server on resume, rename, delete |
| Which chats are running | tmux on the dedicated socket `nostromo`: sessions named `agent-<id8>` | tmux |
| Conversation content | the agent's own storage (for Claude Code: `~/.claude/projects/...`) | the agent |

The server keeps no state of its own. On every request it reads the record directory and asks tmux. Chats started while the server was down are visible as soon as it starts, because the launcher, not the server, created their records.

There is no clutter by construction. The list contains only chats started with `agent` or from the UI. A plain `claude` in a terminal never shows up. Deleting a chat deletes one file.

Data path:

1. `agent` in any directory: creates a chat record, starts a tmux session in that directory running the agent (for Claude Code: `claude --dangerously-skip-permissions --session-id <uuid>`), and attaches the terminal to it.
2. The server (`agent serve`) listens on `127.0.0.1` only. `tailscale serve` exposes it over HTTPS with a Tailscale certificate.
3. The browser opens `https://<machine>.<tailnet>.ts.net`. A drawer lists chats grouped by directory. A chat is a terminal: the server runs `tmux attach` in a PTY and pipes bytes over a WebSocket into xterm.js.
4. After a reboot tmux is empty but the records remain. The chat shows as stopped; the Resume button starts a new tmux session with `claude --dangerously-skip-permissions --resume <uuid>`.

## 2. Language: Go

Three things decide it: single-file install on macOS and Linux, PTY handling without native builds, and launcher plus server as one binary with shared code.

| Criterion | Go | Rust | Node / JS | Python |
| --- | --- | --- | --- | --- |
| `curl \| bash` install | one static file, 8–10 MB | one file, 3–6 MB | needs a specific Node and a native module | needs Python 3 and dependencies |
| PTY for `tmux attach` | `creack/pty`, pure Go, no cgo | `portable-pty` | `node-pty`, native build per platform and Node version | in the standard library |
| WebSocket | one dependency (`coder/websocket`) | `axum` + `tokio` | `ws` | external library |
| Cross-compiling 4 targets | `GOOS/GOARCH` on one runner | needs macOS runners or a cross toolchain | n/a, prebuild matrix instead | n/a |
| Idle memory | ~10 MB | ~3–5 MB | 50–80 MB | 25–40 MB |
| `agent` startup | ~5 ms | ~3 ms | 60–100 ms | 50–80 ms |
| Tests | `go test -race -cover`, `httptest` built in | `cargo test`, more glue for HTTP/async | external runner and mocks | `unittest`/`pytest` |
| Self-update | replace one file | replace one file | update package and native modules | update package and environment |
| Embedding the UI | `embed` in the standard library | `include_dir` | files alongside | files alongside |

Memory and startup numbers are order-of-magnitude estimates, not measurements.

Why not the others:

- **Rust** gives the same result with less memory, but 5 MB does not matter here. Cost: async glue, slower builds, harder macOS cross-compilation.
- **Node** loses on `node-pty`: a native module breaks "download one file and run". Plus 50+ MB for a process that should be invisible.
- **Python** is fine for a prototype, but install and update depend on the system Python, which differs between macOS and Linux.

Go dependencies are limited to four: `creack/pty`, `coder/websocket`, `google/uuid`, `BurntSushi/toml`. HTTP, JSON, embed and tests come from the standard library.

Frontend: one `index.html`, one `app.js`, one `app.css` and vendored xterm.js, all embedded via `embed`. No npm, no build step. All logic (grouping, sorting, statuses) is computed on the server so it is covered by Go tests; the browser only renders.

## 3. Key decisions

### 3.1. Tracking chats started while the server is down

The launcher writes the chat record itself before starting tmux. It generates the UUID and knows the directory and agent type, so no hook is needed for this.

| Option | Works with server down | Survives reboot | Any CLI agent | No clutter |
| --- | --- | --- | --- | --- |
| **A. Launcher writes a record file (chosen)** | yes | yes | yes | yes |
| B. Claude SessionStart hook writes the file | yes | yes | no, Claude only | no: catches every `claude` run, forks included |
| C. Scan `~/.claude/projects/*.jsonl` | yes | yes | no | no: that is the clutter |
| D. Only tmux and its user options | yes | no | yes | yes |

A hook is still used, in a narrow role: keeping the resume id current (see 3.2). It only acts inside chats started via `agent`, because it checks the `NOSTROMO_CHAT_ID` environment variable.

### 3.2. Two ids: chat and agent session

The chat `id` is our UUID and never changes. `resume_id` is the session id inside the agent; resume uses it.

For Claude Code they are equal on first launch: `claude --session-id <id>`. According to the CLI reference, `--resume` reuses the original id unless `--fork-session` is given. Inside a session it can change: `/clear`, `/resume`, fork. The SessionStart hook receives `session_id` and `source` (`startup`, `resume`, `clear`, `compact`, `fork`). The `agent hook claude` command writes the new `session_id` into the record's `resume_id`.

Without the hook everything works until the user runs `/clear`: then Resume returns the conversation from before the clear.

### 3.3. Terminal in the browser

For every WebSocket connection the server runs `tmux attach-session -t <session>` in a PTY and forwards bytes both ways. It is a real terminal: colors, cursor, resize, any key.

Rejected:

- `tmux capture-pane` + `send-keys` on a timer. No PTY, but TUIs render jerkily and with artifacts.
- Control mode (`tmux -C`). A text protocol that must be parsed and the screen rebuilt by hand. Not worth the complexity.

The tmux window size follows the most recently active client (`window-size latest`, the default). Phone and laptop do not fight each other for longer than one keystroke.

### 3.4. HTTPS via Tailscale

The server listens on `127.0.0.1:<port>` over plain HTTP. HTTPS comes from `tailscale serve --bg --https=443 http://127.0.0.1:<port>`: Tailscale obtains and renews the certificate for `<machine>.<tailnet>.ts.net` itself.

| Option | Server code | Certificate renewal | Comment |
| --- | --- | --- | --- |
| **`tailscale serve` (chosen)** | none | automatic | one command at install time; config persists with `--bg` |
| `tailscale cert` + TLS in the server | loading and reloading certs | own logic every 90 days | extra code and an extra point of failure |
| Embedded `tsnet` | a lot | automatic | +20 MB binary, the server becomes its own tailnet node |

Tailnet requirement: MagicDNS and HTTPS certificates enabled in the Tailscale admin console. `agent doctor` checks this.

### 3.5. tmux: rules that must hold

- **Minimum tmux version 3.2.** `remain-on-exit failed` is required.
- **Dedicated tmux socket `nostromo`** (`tmux -L nostromo`), not the default one. Chat sessions live in their own tmux server: `tmux kill-server` on the user's own tmux does not touch agents, and a user `~/.tmux.conf` does not change how chats look in the browser. Chats are listed with `agent ls` (or `tmux -L nostromo ls`). The socket name is configurable; tests use a random one.
- **Own tmux config.** Every tmux invocation passes `-f ~/.nostromo/tmux.conf`, which `agent setup` writes from an embedded template (the file is only read when the tmux server starts). It sets: `mouse on`, `history-limit 50000`, `default-terminal tmux-256color`, `escape-time 10`, `status off`, `set-titles off`. If the file is missing, the launcher writes it before starting tmux.
- **Launching from inside tmux.** If `$TMUX` points at the nostromo socket (running `agent` inside a chat), use `switch-client`. Inside any other tmux, attach normally; this nests tmux, and the launcher prints a one-line hint about the doubled prefix.
- **The agent command runs through the user's login shell**: `$SHELL -lic 'exec <command>'`. Otherwise a chat started by the server under launchd or systemd would not find `claude` in `PATH` or see variables from `.zshrc`. The adapter option `shell_wrap = false` runs the command directly.
- **`remain-on-exit failed`** on the session. If the agent exits non-zero, the pane stays and the error is visible in the browser. On a clean exit the session closes.
- **Restarting the server must not kill tmux.** The systemd unit must have `KillMode=process`, the launchd plist `AbandonProcessGroup = true`. A dedicated acceptance test covers this, because update safety depends on it.

## 4. Files and data model

Everything lives under `~/.nostromo/`, identically on macOS and Linux. Removing the directory and the service removes the system entirely.

```text
~/.nostromo/
  bin/agent          current binary; ~/.local/bin/agent is a symlink to it
  bin/agent.prev     previous version, for rollback
  config.toml        settings
  tmux.conf          tmux config for the nostromo socket
  chats/<id>.json    one record per chat
  chats/.lock        lock file for record changes
  logs/server.log    server log, size-based rotation (5 MB, 3 files)
  logs/hook.log      hook errors
  update.json        cache of the last update check
```

### 4.1. Chat record

```json
{
  "v": 1,
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "agent": "claude",
  "cwd": "/Users/me/projects/shop-api",
  "title": "",
  "created_at": "2026-11-16T10:00:00Z",
  "last_started_at": "2026-11-16T10:00:00Z",
  "resume_id": "550e8400-e29b-41d4-a716-446655440000",
  "tmux_session": "agent-550e8400"
}
```

Rules:

- Records are written atomically: temp file in the same directory, then `rename`.
- Every change to an existing record happens under `flock` on `chats/.lock`: read, modify, write. There are three writers: launcher, hook, server.
- Unknown JSON fields are preserved on rewrite, so an older version does not damage records of a newer one.
- A corrupt file never crashes the server or the launcher: it is skipped and reported by `agent doctor`.
- `v` is the schema version. `agent setup --upgrade` runs migrations (section 10).

### 4.2. Chat status

Status is not stored; it is computed on every request from a single tmux call:

```text
tmux -L nostromo list-panes -a -F '#{session_name}\t#{@nostromo_chat}\t#{pane_dead}\t#{pane_dead_status}\t#{session_attached}'
```

| Status | Condition | What the user sees |
| --- | --- | --- |
| `running` | session exists, pane alive | terminal |
| `failed` | session exists, pane dead (`pane_dead = 1`) | last screen with the error, exit code, Resume and Delete buttons |
| `stopped` | no session | empty view with a Resume button |

tmux answering "no server running" (or a missing socket) means an empty list, not an error. A tmux session with `@nostromo_chat` but no record is ignored by the server and reported by `agent doctor`.

### 4.3. tmux names

- Session name: `agent-` + the first 8 characters of `id`.
- The session gets the user option `@nostromo_chat = <full id>`. Matching uses this option; the name is for humans only.
- The agent environment gets `NOSTROMO_CHAT_ID` and `NOSTROMO_HOME`.

### 4.4. Config

```toml
port = 8787
tmux_socket = "nostromo"    # tmux -L <name>
default_agent = "claude"
repo = "aist-dev/nostromo"  # where releases come from
check_updates = true        # ask GitHub for a new version once a day
auto_update = false
allowed_users = []          # Tailscale logins; filled in by setup, see 7.4
peers = []                  # other machines for the UI switcher, e.g. ["mac-mini", "linux-box"]

[agents.claude]
start      = "claude --dangerously-skip-permissions --session-id {id}"
resume     = "claude --dangerously-skip-permissions --resume {resume_id}"
hooks      = "claude"       # built-in hook installer for this agent
shell_wrap = true           # false = run the command directly, without a login shell
```

The default Claude adapter starts Claude Code with `--dangerously-skip-permissions`, in both templates, so chats from the terminal and from the browser behave the same. Consequence: anyone who can reach the UI controls an agent that does not ask for confirmation, which is why `allowed_users` and Tailscale access matter (section 7.4).

The config is read on every command and at server start. A missing file means defaults.

## 5. The `agent` command

`agent` is a binary on `PATH`, not an alias or a shell function. It works the same in bash, zsh, fish and scripts, and its behavior is covered by Go tests.

| Command | What it does |
| --- | --- |
| `agent` | new chat in the current directory with the default agent, then attach |
| `agent -a <name>` | same with another agent from the config |
| `agent --cwd <path>`, `--title <text>`, `--detach` | directory, title, do not attach |
| `agent ls` | chats grouped by directory, with status |
| `agent attach <id>` | attach the terminal to a running chat |
| `agent resume <id>` | resume a stopped chat and attach |
| `agent stop <id>` | end the tmux session, keep the record |
| `agent rm <id>` | end the session and delete the record |
| `agent serve` | run the server (the service calls it, not a human) |
| `agent setup`, `doctor`, `update`, `uninstall`, `version` | install, check, update (sections 9 and 10) |
| `agent hook <adapter>` | internal: called by agent hooks |

Wherever `<id>` is expected, an unambiguous prefix is accepted.

There is intentionally no "continue the last chat in this directory" shortcut: several chats per directory are normal, and picking the "last" one would be a guess.

### 5.1. Starting a new chat: exact sequence

One function, `chat.Start`, runs this sequence. Both the launcher and the server call it, so terminal chats and browser chats are indistinguishable.

1. Check: the directory exists, the adapter exists in the config, tmux is present and at least 3.2.
2. Generate `id` (UUID v4). Build the record with `resume_id = id`. Write the file.
3. Build the agent command from the `start` template, substituting shell-escaped values.
4. Wrap it: `env NOSTROMO_CHAT_ID=<id> NOSTROMO_HOME=<path> $SHELL -lic 'exec <command>'` (or without the shell when `shell_wrap = false`).
5. Ensure `~/.nostromo/tmux.conf` exists. With one tmux call, create the session and set its options so they apply before the command can exit:

```text
tmux -L nostromo -f ~/.nostromo/tmux.conf \
     new-session -d -s agent-<id8> -c <cwd> -x <cols> -y <rows> '<wrapped command>' \; \
     set-option    -t agent-<id8> @nostromo_chat <id> \; \
     set-option -w -t agent-<id8> remain-on-exit failed
```

6. If tmux fails: delete the record, print the reason, exit code 1.
7. Attach. With `--detach` or without a TTY: print `id`. Inside a nostromo tmux (`$TMUX` points at the nostromo socket): `tmux switch-client -t ...`. Otherwise: `exec tmux -L nostromo attach-session -t ...`.

Size `-x/-y`: the launcher uses the current terminal size; the server uses 120×40 until the first browser connects.

### 5.2. Resume

1. Status `running`: start nothing, just attach.
2. Status `failed`: `tmux kill-session` first.
3. Check that `cwd` exists. If not, fail with a clear message and leave the record untouched.
4. Same sequence as 5.1 with the `resume` template and the current `resume_id`. Update `last_started_at`.

### 5.3. Command name

On a machine where `agent` used to be a shell alias (for example `alias agent='claude --dangerously-skip-permissions'`), the binary replaces it with the same permission level (section 4.4).

`agent setup` handles name conflicts in two ways:

- **A shell alias or function named `agent`** in rc files (`.bashrc`, `.zshrc`, `config.fish`). An alias shadows the binary in interactive shells, so `setup` backs up the rc file (`<file>.nostromo.bak`), comments the line out with the prefix `# nostromo: disabled ` and prints what it changed. `agent uninstall` restores it. `agent doctor` runs `type agent` in a login shell and reports if an alias reappears.
- **Another program** at `~/.local/bin/agent` or earlier in `PATH`. `setup` never removes other programs: it stops and suggests removing it manually or using another symlink name (for example `ag`) via `NOSTROMO_CMD=ag`. The binary does not depend on the name it was invoked as.

Agent hooks and service files never call `agent` by name: aliases do not work in non-interactive shells, and the `PATH` of a service or hook can differ. They always use the absolute path of `~/.nostromo/bin/agent`, expanded at write time.

## 6. Agent adapters

An adapter is a config block with two command templates, `start` and `resume`. A new CLI agent is added without code changes.

Template placeholders: `{id}`, `{resume_id}`, `{cwd}`. Values are shell-escaped. An unknown placeholder is an error at config load time, not at chat start.

```toml
[agents.codex]              # example; verify exact commands against the agent's docs when adding
start  = "codex"
resume = "codex resume --last"
```

An agent that cannot take an id at startup still works: `resume_id` stays equal to `id`, and the `resume` template uses whatever the agent supports (for example "last session in this directory"). Resume precision is then limited by the agent; the README notes this.

### 6.1. Claude Code hooks

Hooks are optional and do two things: keep `resume_id` current and set the chat title from the first prompt.

`agent setup` adds this to `~/.claude/settings.json` (absolute path to the binary, expanded at write time, see 5.3):

```json
{
  "hooks": {
    "SessionStart":     [{ "hooks": [{ "type": "command", "command": "/home/me/.nostromo/bin/agent hook claude", "timeout": 5 }] }],
    "UserPromptSubmit": [{ "hooks": [{ "type": "command", "command": "/home/me/.nostromo/bin/agent hook claude", "timeout": 5 }] }]
  }
}
```

`agent hook claude` behavior:

1. If `NOSTROMO_CHAT_ID` is unset, exit 0 immediately. A plain `claude` outside `agent` is unaffected.
2. Read JSON from stdin. `hook_event_name` selects the branch.
3. `SessionStart`: if `session_id` differs from the record's `resume_id`, update the record. Do not update for `source = "fork"`: a fork is a different conversation.
4. `UserPromptSubmit`: if `title` is empty, store the first 60 characters of `prompt` as a single line.
5. Always exit 0 with empty stdout. `UserPromptSubmit` output goes into the model context, so nothing may be written there.
6. Any error (missing record, bad JSON, lock held longer than 1 second) is logged to `logs/hook.log`, exit 0. The hook must never get in the agent's way.

Rules for editing `settings.json`:

- Merge, never overwrite: other hooks and settings are preserved.
- Idempotent: our hook is recognized by a command ending in `/.nostromo/bin/agent hook <adapter>`; re-running `setup` duplicates nothing.
- Before the first change, copy to `settings.json.nostromo.bak`.
- Invalid JSON: leave the file alone, print a warning and the snippet to paste manually.
- `agent uninstall` removes only our entries.

### 6.2. Out of scope for the first version

A "waiting for input" status and notifications. The extension point exists: the same `agent hook` can handle `Notification` and `Stop` events and write a `state` field into the record. It can be added later without architectural changes.

## 7. Server

The server is a thin HTTP layer over three packages: record store, tmux wrapper, adapters. Apart from open WebSocket connections it has no state.

### 7.1. API

| Method and path | Purpose |
| --- | --- |
| `GET /api/state` | everything the drawer needs: host, version, agents, chat groups, recent directories, available update |
| `POST /api/chats` | new chat: `{"agent": "claude", "cwd": "/path"}` → chat record |
| `POST /api/chats/{id}/resume` | resume |
| `POST /api/chats/{id}/stop` | end the tmux session, keep the record |
| `PATCH /api/chats/{id}` | rename: `{"title": "..."}` |
| `DELETE /api/chats/{id}` | end the session if any and delete the record |
| `GET /api/chats/{id}/tty` | WebSocket terminal |
| `POST /api/chats/{id}/scroll` | tmux history scrolling: `{"dir": "up" \| "down" \| "exit"}` |
| `GET /api/dirs?path=...` | subdirectories for the new-chat folder picker |
| `POST /api/update` | start an update (section 10) |
| `GET /healthz` | version and liveness; used by the updater |

`GET /api/state` response:

```json
{
  "host": "macbook",
  "version": "1.4.0",
  "update_available": "1.5.0",
  "agents": ["claude"],
  "default_agent": "claude",
  "peers": [{"name": "linux-box", "url": "https://linux-box.tail1234.ts.net"}],
  "recent_dirs": ["/Users/me/projects/shop-api"],
  "groups": [
    {
      "cwd": "/Users/me/projects/shop-api",
      "name": "shop-api",
      "chats": [
        {"id": "550e8400-...", "title": "Fix payments", "agent": "claude",
         "status": "running", "exit_code": null, "attached": 1,
         "created_at": "...", "last_started_at": "..."}
      ]
    }
  ]
}
```

Sorting happens on the server. Groups: by the latest activity of any chat in the group, newest first. Within a group: `running` and `failed` first, then `stopped`; within each block by `last_started_at`, newest first.

The response supports `ETag` / `If-None-Match`: the browser polls `state` every 2 seconds and almost always gets `304`.

Errors share one format: `{"error": {"code": "...", "message": "..."}}`. Codes: `not_found`, `bad_request`, `already_running`, `not_running`, `cwd_missing`, `unknown_agent`, `tmux_error`, `forbidden`.

### 7.2. WebSocket terminal

1. Connect: `GET /api/chats/{id}/tty?cols=120&rows=40`.
2. Chat `stopped`: the server closes the socket with code `4404`. The UI shows Resume.
3. Otherwise the server runs `tmux -L nostromo attach-session -t <session>` in a PTY with `TERM=xterm-256color` and the requested size.
4. Binary frames carry terminal bytes both ways.
5. Text frames from the client are JSON commands. Currently one: `{"type": "resize", "cols": 100, "rows": 30}`.
6. The `tmux attach` process exits (session closed): the server sends a text frame `{"type": "exit"}` and closes with code `4410`. The UI reloads `state`.
7. Client disconnects: the server terminates the `tmux attach` process. The session and the agent keep running.
8. Ping every 30 seconds; no reply within 60 seconds closes the connection.

Scrolling: `mouse on` in the nostromo tmux config makes the desktop mouse wheel scroll tmux history. For phones there are buttons calling `/scroll`: the server runs `tmux copy-mode` and `send-keys -X halfpage-up` / `halfpage-down` / `cancel`.

### 7.3. Folder picker

`GET /api/dirs?path=~/projects` returns `{"path": "/abs", "parent": "/abs/..", "dirs": ["a", "b"]}`. Directories only, hidden ones excluded (`all=1` includes them), symlinks to directories included. Without `path`: the home directory.

### 7.4. Security

The server grants terminal access to the machine, so the rules are strict and each one is covered by a test.

- Listen on `127.0.0.1` only. Any other address requires the explicit `--listen` flag and logs a warning.
- `Host` check: only `localhost`, `127.0.0.1` and the machine's tailnet name are allowed. Anything else gets `403`. This defends against DNS rebinding.
- `Origin` check on every request except side-effect-free `GET`, and on the WebSocket upgrade: the origin must match `Host` or `X-Forwarded-Host`.
- `allowed_users`: requests proxied by `tailscale serve` carry the Tailscale identity header (`Tailscale-User-Login`; verify against the Tailscale Serve docs during implementation). When the list is non-empty, the login must be in it, otherwise `403`; requests without the header coming through the proxy are rejected too. `agent setup` fills the list with the login of the current Tailscale user (`tailscale status --json`). When the list is empty, the server works but logs a warning at startup, and `agent doctor` reports it.
- Tailscale Funnel (public internet) is never used. `agent doctor` warns if Funnel is enabled for this port.
- Limits: request body 1 MB, WebSocket frame 1 MB, header read timeouts.
- Only agents from the config can be started. No arbitrary command can be passed through the API.

## 8. UI

Two parts: a drawer listing chats by directory and a main area with the selected chat's terminal. One page, no framework, no build step. All UI text is English.

### 8.1. Layout

| Part | Desktop (900 px and wider) | Phone |
| --- | --- | --- |
| Drawer | pinned on the left, 300 px | slides over the content from a hamburger button, closes after a chat is chosen |
| Main area | terminal fills the remaining width | terminal fills the screen |
| Key bar and input line | hidden (there is a keyboard) | pinned at the bottom, above the on-screen keyboard |

### 8.2. Drawer

- Header: machine name, switcher to other machines from `peers` (plain links), "New chat" button.
- Groups by directory: directory name large, full path small, a "+" button that creates a chat in that directory without a dialog.
- Chat row: status dot, title (if empty: "Chat from Nov 16, 10:00"), relative time.
- Stopped chats are dimmed. A group where every chat is stopped is collapsed; collapsed state is kept in `localStorage`.
- A filter field at the top matches title and path.

### 8.3. Main area

| Chat status | What is shown |
| --- | --- |
| `running` | xterm.js terminal connected to `/tty` |
| `failed` | terminal with the last screen (read-only), a banner "Exited with code N", Resume and Delete buttons |
| `stopped` | empty state: title, directory, last start time, Resume and Delete buttons |
| none selected | a hint and a "New chat" button |

Top bar: hamburger (phone), title (tap to rename), status, menu: Stop, Delete, Rename. Deleting a running chat asks for confirmation; deleting a stopped chat happens at once with a 5-second undo (the record is physically deleted when the undo window ends).

### 8.4. Phone input

- Key bar: Esc, Tab, Shift+Tab, Ctrl+C, arrows, Enter, scroll up and down.
- The input line is a regular text field, so autocorrect and dictation work. Sending writes the text, then Enter separately after 100 ms; otherwise a TUI may treat it as a paste with a newline.
- Page height follows `visualViewport` so the on-screen keyboard does not cover the terminal.

### 8.5. Behavior

- The chat address is in the hash: `#<id>`. A link opens a specific chat; Back works.
- WebSocket drop: automatic reconnect after 1, 2, 5 seconds, then every 5. Screen contents come back from tmux on re-attach.
- A change of `version` in `state` (server updated): the page reloads itself.
- "New chat" dialog: agent choice (when there is more than one), recent directories, directory navigation via `/api/dirs`, a field for typing a path.
- Fonts and xterm.js are embedded in the binary. The page makes no external requests.

## 9. Installing on a new machine

Installation is one command. The bash script does the minimum: download and verify the binary. Everything else is `agent setup`, Go code covered by tests.

```sh
curl -fsSL https://raw.githubusercontent.com/aist-dev/nostromo/main/install.sh | bash
```

### 9.1. Prerequisites

The installer checks these but installs none of them and never calls `sudo`. The README lists every manual step with the exact command.

| Requirement | How to install | Required |
| --- | --- | --- |
| tmux 3.2 or newer | `brew install tmux` / `sudo apt install tmux` | yes |
| Tailscale, logged in | from the Tailscale website | for access from other devices |
| MagicDNS and HTTPS certificates in the tailnet | Tailscale admin console, once per tailnet | for HTTPS |
| Permission to manage `tailscale serve` without sudo (Linux) | `sudo tailscale set --operator=$USER` | for automatic HTTPS setup |
| `claude`, logged in | per the Claude Code docs | for Claude chats |
| `curl`, `tar`, `sha256sum` or `shasum` | part of the system | yes |

### 9.2. Step A: `install.sh`

1. `set -euo pipefail`. All code lives in a `main` function called on the last line, so a truncated download never runs half a script.
2. Detect the OS (`uname -s`: `Darwin` → `darwin`, `Linux` → `linux`) and architecture (`uname -m`: `x86_64` → `amd64`, `arm64` and `aarch64` → `arm64`). Anything else is a clear error.
3. Version: `NOSTROMO_VERSION` or the latest release.
4. Download `agent_<os>_<arch>.tar.gz` and `checksums.txt` from GitHub Releases into a temp directory.
5. Verify SHA-256. On mismatch stop without changing anything.
6. Unpack into `~/.nostromo/bin/agent`, `chmod 755`.
7. `exec ~/.nostromo/bin/agent setup`.

Running `install.sh` again on an installed system is safe: it installs the latest version and runs `setup` again.

### 9.3. Step B: `agent setup`

Every step is idempotent and prints its result. A failing optional step (Tailscale, hooks) does not stop the installation: at the end it prints what is left to do manually, with the exact command.

1. **Directories and config.** Create `~/.nostromo/{bin,chats,logs}`. Write the default `config.toml` only if missing. Write `tmux.conf`.
2. **Command on `PATH`.** Resolve name conflicts per 5.3: comment out an `agent` alias in rc files; stop if another program occupies the symlink location. Symlink `~/.local/bin/agent`. If `~/.local/bin` is not on `PATH`, add a line to the current shell's rc file (`.zshrc`, `.bashrc`, `config.fish`) between the markers `# >>> nostromo >>>` and `# <<< nostromo <<<`.
3. **Checks.** tmux version. Agent commands from the config resolve in a login shell (stderr of the TTY-less login shell is discarded). A missing agent is a warning, not an error.
4. **Agent hooks** per section 6.1.
5. **Service.**
   - macOS: `~/Library/LaunchAgents/com.nostromo.server.plist` with `RunAtLoad`, `KeepAlive`, `AbandonProcessGroup`; loaded via `launchctl bootstrap gui/<uid>`.
   - Linux: `~/.config/systemd/user/nostromo.service` with `Restart=on-failure` and `KillMode=process`; `systemctl --user enable --now nostromo`; then `loginctl enable-linger <user>` so the service starts without a login. If enabling linger is refused, print the command for the user to run.
6. **Wait** for `GET /healthz` to report the expected version, up to 10 seconds.
7. **HTTPS.** If Tailscale is running: `tailscale serve --bg --https=443 http://127.0.0.1:<port>`. If 443 already carries another serve config, leave it and use `--https=8443`. Fill `allowed_users` with the current Tailscale login if the list is empty.
8. **Summary.** Print `https://<machine>.<tailnet>.ts.net` (from `tailscale status --json`) and the local `http://127.0.0.1:<port>`.
9. **`agent doctor`.**

### 9.4. `agent doctor`

Prints a checklist with a mark and, for failures, the fix command: tmux version; binary and symlink; `PATH`; `agent` in a login shell resolves to our binary, not an alias or another program; service running; `/healthz` answers; Tailscale running; serve configured for our port; Funnel off; `allowed_users` non-empty; HTTPS address answers; hooks installed; agent commands found; corrupt chat records; tmux sessions without records.

### 9.5. Uninstall

`agent uninstall`: stop and remove the service, remove our `tailscale serve` config, remove hooks, the symlink and the rc block, restore aliases disabled by `setup` (lines prefixed with `# nostromo: disabled `). `~/.nostromo` is removed only with `--purge`. Running tmux sessions are left alone.

### 9.6. Installation acceptance

On a clean machine after one command: `agent doctor` is all green; `agent` in a new terminal opens Claude Code in tmux; the chat is visible in a phone browser over the HTTPS address; after a reboot the address works without logging in (Linux) or after login (macOS), and the chat shows as stopped.

## 10. Updates and releases

An update replaces one file and restarts the server. Running agents are not interrupted, because they live in tmux, not in the server.

### 10.1. `agent update`: exact sequence

1. Find the latest version: `GET https://api.github.com/repos/aist-dev/nostromo/releases/latest`. Compare with the binary's version by semver. Equal or older: "already up to date", exit 0.
2. Download the archive for this OS and architecture and `checksums.txt` into a temp directory.
3. Verify SHA-256. On mismatch stop.
4. Unpack into `~/.nostromo/bin/agent.new`. Run `agent.new version`: the output must match the expected version. This catches a broken or foreign binary before replacement.
5. Copy the current `agent` to `agent.prev`. Atomically rename `agent.new` to `agent`.
6. Run the new binary: `agent setup --upgrade`. It runs record and config migrations and refreshes the service file and hooks if their templates changed. It does not touch rc files or `tailscale serve`.
7. Restart the service: `launchctl kickstart -k gui/<uid>/com.nostromo.server` or `systemctl --user restart nostromo`.
8. Verify: `GET /healthz` must report the new version within 10 seconds.
9. Verification failed: roll back — put `agent.prev` back, restart the service, print the reason, exit 1.

Flags: `--check` only reports whether a new version exists; `--version vX.Y.Z` installs a specific one; `--rollback` restores `agent.prev`.

### 10.2. When updates run

| Way | Behavior |
| --- | --- |
| Manual | `agent update` in a terminal |
| From the UI | the server checks releases once a day (cached in `update.json`); the drawer shows "Update to 1.5.0"; the button calls `POST /api/update` |
| Automatic | only with `auto_update = true`; the server starts the update itself after the daily check |

`POST /api/update` does not update inside the server process. It starts a separate `agent update` process in a new session (`setsid`) and returns `202` immediately; otherwise restarting the server would kill its own updater. The page waits for `version` in `state` to change and reloads.

`check_updates = false` disables all requests to GitHub. The repository is public, so no token is needed; `NOSTROMO_GITHUB_TOKEN` is used if set (for forks in private repositories or rate limits).

### 10.3. Cross-version compatibility

- Launcher, server, hook and UI are one binary, so there is no protocol skew between them.
- The record format changes only with a bump of `v` and a migration in `setup --upgrade`. Migrations are idempotent and copy `chats/` to `chats.bak-v<N>/` first.
- A new version reads records of all older versions. An old version after a rollback skips records with an unknown `v` and never rewrites them.
- Config fields may not be removed or renamed without reading the old name for at least one minor version.
- tmux session names, the socket name and the `@nostromo_chat` option are part of the contract: changing them requires migrating running sessions.

### 10.4. Releases

1. The version comes from the git tag `vX.Y.Z` and is embedded via `-ldflags`.
2. A tag triggers GitHub Actions: tests on `ubuntu-latest` and `macos-latest`, then GoReleaser.
3. GoReleaser builds four archives — `agent_darwin_arm64`, `agent_darwin_amd64`, `agent_linux_amd64`, `agent_linux_arm64` — plus `checksums.txt`, and publishes a GitHub Release.
4. `install.sh` lives on `main` and can always install any published release.
5. `CHANGELOG.md` marks for every version whether it contains a data migration.

Archive and checksum file names are a contract between the release, `install.sh` and `agent update`. They are defined in one place in the GoReleaser config and checked by an updater test.

## 11. Tests

Three levels. Every package is written together with its tests; a stage is not done until the tests are green on Linux and macOS.

### 11.1. What makes the code testable

- All external calls go through interfaces: `Tmux` (running tmux commands), `Clock`, `HTTPClient` (GitHub), `ServiceManager` (launchd/systemd), `Tailscale`. Unit tests use fakes.
- The home directory (`NOSTROMO_HOME`) and the tmux socket are parameters. A test never touches the real `~/.nostromo` or the user's tmux sessions.
- A fake agent in `testdata/fakeagent`: a small program that writes its arguments and environment to a file, prints a prompt, echoes input as `echo:<line>`, and exits with code N on `exit N`. It stands in for everything Claude Code does in real life.

### 11.2. Unit tests (no tmux, fast)

| Package | What is tested |
| --- | --- |
| `store` | create, read, update, delete; atomicity; locking under concurrent writes from 20 goroutines; unknown fields preserved; corrupt files skipped; prefix lookup and ambiguous prefixes |
| `config` | defaults; parsing; unknown template placeholder is an error; missing file |
| `adapter` | substitution and escaping (spaces, quotes, `$`, `;` in paths); login-shell wrapping; `shell_wrap = false` |
| `tmux` | command argument building (socket and config flags); parsing `list-panes` output; "no server running" as an empty list; version parsing (`3.2a`, `3.5`, `next-3.6`) |
| `chat` | `Start` and `Resume` against a fake `Tmux`: call order, record rollback on tmux error, missing directory, resuming running and failed chats |
| `state` | status computation; grouping and sorting; ETag changes only when content changes |
| `server` | every endpoint via `httptest`: success and every error code; Host and Origin checks; `allowed_users`; body limit |
| `hook` | `SessionStart` samples (every `source` value) and `UserPromptSubmit`; without `NOSTROMO_CHAT_ID` it does nothing; always exit 0 and empty stdout |
| `setup` | `settings.json` merge (empty, with foreign hooks, already ours, corrupt); absolute path in hooks; plist and unit generation compared with golden files; rc edits are idempotent; an `agent` alias is commented out and restored by `uninstall`, other aliases untouched |
| `update` | fake GitHub on `httptest`: no new version; successful update; bad checksum; binary with the wrong version; rollback after failed verification; semver comparison |
| `migrate` | every migration on samples of old records; a second run changes nothing |

### 11.3. Integration tests (real tmux, own socket)

Built with the `integration` tag. Each test creates its own socket `tmux -L nostromo-test-<random>` and its own temporary `NOSTROMO_HOME`, and kills its tmux server at the end.

1. New chat: record created, session exists, status `running`, the fake agent got `--session-id` and `NOSTROMO_CHAT_ID`, working directory correct.
2. Agent exits 0: no session, status `stopped`, record kept.
3. Agent exits 3: status `failed`, code 3, last screen readable via `/tty`.
4. Nonexistent agent command: status `failed`, error text visible.
5. Resume: the fake agent got `--resume <resume_id>`; `last_started_at` updated.
6. **Chat created while the server is down**: the launcher ran, the server starts later and shows the chat right away.
7. **Reboot**: kill the whole tmux server — all chats `stopped`; Resume works.
8. **Server restart leaves agents alone**: stop and start the server — same session, same agent PID.
9. Terminal: connect to `/tty`, send `hello` and Enter, read `echo:hello`.
10. Resize: the `resize` command changes the size the agent sees (`stty size`).
11. Two clients on one chat: both receive output; disconnecting one does not affect the other.
12. Client disconnect: the `tmux attach` process ends, the session lives.
13. Deleting a running chat: session killed, file gone, the connected `/tty` got `exit`.
14. The hook changes `resume_id`: the next Resume uses the new id.
15. Launcher inside a nostromo tmux session: `switch-client` is used, no nesting.
16. Directory with spaces and non-ASCII characters in the path.

### 11.4. UI end-to-end test

One Playwright scenario in CI (Linux, phone and desktop emulation): open the page, see groups, create a chat via the dialog, type into the terminal and see the echo, stop, Resume, rename, delete. It also checks that the browser console has no errors. This is the only place Node is needed, and only in CI.

### 11.5. CI

- Matrix `ubuntu-latest` and `macos-latest`; tmux installed via the package manager.
- `go vet`, `staticcheck`, `go test -race -coverprofile` for unit and integration tests.
- Coverage threshold: 90% of lines across `internal/...` packages; the build fails below it.
- `shellcheck` for `install.sh` and an installer test in a clean container: a local HTTP server instead of GitHub, checking a successful install and refusal on a bad checksum.

### 11.6. Not covered by tests, checked manually before a release

Real `tailscale serve` with a certificate; service start after a real reboot on macOS and Linux; typing and scrolling comfort on a real phone; real Claude Code with hooks. The checklist lives in `docs/release-checklist.md`.

## 12. Implementation stages

Nine stages, each ending in a working, tested result. The order makes `agent` usable without the server after stage 3.

### 12.1. Repository layout

```text
cmd/agent/main.go          subcommand parsing, nothing else
internal/config/           config and defaults
internal/store/            chat records
internal/adapter/          agent command templates
internal/tmux/             tmux wrapper (interface + implementation)
internal/chat/             Start, Resume, Stop, Delete
internal/state/            statuses, grouping, sorting
internal/server/           HTTP, WebSocket, PTY
internal/server/web/       index.html, app.js, app.css, vendor/xterm (embed)
internal/hook/             agent hook handlers
internal/setup/            setup, doctor, uninstall, services, Tailscale
internal/update/           update check and install
internal/migrate/          record and config migrations
testdata/fakeagent/        fake agent for tests
e2e/                       Playwright scenario
install.sh
.goreleaser.yaml
.github/workflows/ci.yml, release.yml
docs/release-checklist.md
CHANGELOG.md, README.md
```

### 12.2. Rules for the implementer

- Tests are written in the same change as the code.
- No dependencies beyond the four named in section 2 without explicit agreement.
- Before using tmux, Claude Code and Tailscale flags, check their current documentation or `--help`; the commands in this plan are guidance, not guarantees.
- Unit tests must not require tmux; integration tests use only their own socket.
- Everything in the repository (code, comments, docs, commit messages) is in English.

### 12.3. Stages

- [ ] **Stage 1. Skeleton.** Go module, `cmd/agent` with the `version` subcommand, `config` and `store` packages with tests, CI with a Linux and macOS matrix, coverage threshold. Done when `go test ./...` is green on both OSes.
- [ ] **Stage 2. tmux and adapters.** `adapter` and `tmux` packages, fake agent, tmux version check. Done when unit tests pass and an integration test creates and finds a session on a separate socket.
- [ ] **Stage 3. Launcher.** `chat` and `state` packages; `agent`, `ls`, `attach`, `resume`, `stop`, `rm`. Done when integration tests 1–7, 15, 16 pass, and manually: `agent` in any directory opens Claude Code in tmux, and after `tmux -L nostromo kill-server` `agent resume <id>` continues the conversation.
- [ ] **Stage 4. Server API.** `agent serve`, every endpoint except `/tty` and `/update`; security checks from 7.4. Done when `server` tests cover every endpoint and error code, and integration test 8 passes.
- [ ] **Stage 5. Terminal.** PTY and WebSocket, `/scroll`. Done when integration tests 9–13 pass.
- [ ] **Stage 6. UI.** The page per section 8, embedded in the binary. Done when the Playwright scenario passes at both screen sizes, and a manual check on a real phone: open a chat, send a message, scroll history, Resume.
- [ ] **Stage 7. Claude Code hooks.** `agent hook claude`, `settings.json` merge. Done when `hook` tests and integration test 14 pass, and manually: after `/clear` in Claude Code the record's `resume_id` changes.
- [ ] **Stage 8. Installation.** `setup`, `doctor`, `uninstall`, launchd and systemd services, `tailscale serve`, `install.sh`. Done when the container installer test passes and the acceptance in 9.6 passes on one Mac and one Linux machine.
- [ ] **Stage 9. Releases and updates.** GoReleaser, release workflow, `agent update` with rollback, update checks in the server and the UI button, migrations. Done when `v0.1.0` and `v0.1.1` are published; `agent update` moves from the first to the second on both OSes with an agent running, without interrupting it; a deliberately broken release rolls itself back.

### 12.4. Out of scope for the first version

"Waiting for input" status and notifications; a combined chat list across machines; viewing a stopped chat's history without Resume; multiple windows per tmux session; Windows.

## 13. Risks and decisions

### 13.1. Risks

| Risk | Consequence | Mitigation |
| --- | --- | --- |
| Scrolling and typing in xterm.js on a phone over tmux | long answers are awkward to read | scroll buttons and an input line; manual check in stage 6 before any other UI polish |
| Claude Code changes flags or hook behavior | start, resume or `resume_id` tracking breaks | everything Claude-specific lives in config templates and the `hook` package; a release-checklist item |
| Without the hook `resume_id` goes stale after `/clear` | Resume opens the pre-clear conversation | hooks are installed by default; `doctor` reports when they are missing |
| Slow or noisy login-shell rc files | chats start slowly or with extra output | adapter option `shell_wrap = false` runs the command directly; setup/doctor discard login-shell stderr |
| A service restart kills tmux | an update interrupts every agent | `KillMode=process`, `AbandonProcessGroup`; integration test 8 and stage 9 acceptance |
| `tailscale serve` needs permissions or the CLI is not on `PATH` | HTTPS is not configured automatically | also look for `/Applications/Tailscale.app/Contents/MacOS/Tailscale`; on refusal print the exact command |
| On macOS a LaunchAgent starts only after the user logs in | the server is unreachable after a reboot until login | accepted limitation, documented in the README |
| Unsigned binary on macOS | the system may refuse to run it | check in stage 9 on a clean Mac; add signing to the release workflow if needed |
| UI access equals terminal access, and the Claude adapter runs without permission prompts | any device in the tailnet controls the machine | `allowed_users` filled by setup, Tailscale ACLs, Funnel never enabled |

### 13.2. Decisions

- Project name `nostromo`, data directory `~/.nostromo`, command `agent`.
- Dedicated tmux socket `nostromo` with its own config.
- `agent` always creates a new chat; there is no "continue last chat" shortcut.
- Deleting a chat does not touch the agent's own history (for Claude Code, `~/.claude`); it stays reachable with a plain `claude --resume`.
- Auto-update is off by default; the UI announces new versions.
- A plain `claude` started without `agent` is not tracked at all.
- The repository is public; install and update need no token.
- `allowed_users` is filled automatically by `setup`; an empty list works but produces warnings.
- Coverage threshold stays at 90% for `internal/...`.

### 13.3. Alternatives worth knowing

Claude Code has its own remote control and background sessions. This project differs in three ways: it works with any CLI agent, keeps everything in tmux on your own machines, and provides its own chat list without clutter.

### 13.4. References

- Claude Code CLI reference — `--session-id`, `--resume`, `--fork-session`: https://docs.claude.com/en/docs/claude-code/cli-reference
- Claude Code hooks — `SessionStart`, `UserPromptSubmit`, settings format: https://docs.claude.com/en/docs/claude-code/hooks
- Tailscale `serve` command — `--bg`, `--https`, proxying to `127.0.0.1`: https://tailscale.com/docs/reference/tailscale-cli/serve
