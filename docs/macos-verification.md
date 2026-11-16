# macOS verification

Everything in this repository was built and tested on Linux. This is the plan for
verifying it on a Mac: run each step in order, fix what fails, and record the result
at the bottom. Fixes follow the same rules as the rest of the project (tests in the
same change, `docs/PLAN.md` updated first when behavior changes).

## 0. Prerequisites

```sh
xcode-select --install                 # git, cc (only if missing)
brew install go tmux node              # Go 1.25+, tmux 3.2+, Node for the browser test
go version && tmux -V && node --version
```

Claude Code and Tailscale (the app from the Mac App Store or tailscale.com) must be
installed and logged in for steps 5–7.

## 1. Build and unit tests

```sh
go vet -tags integration ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -tags integration ./...
go test -race ./...
```

Things most likely to differ from Linux and worth reading first if a test fails:

- `internal/store`: `syscall.Flock` on APFS.
- `internal/adapter`: `/bin/sh` is bash in POSIX mode; the default login shell is zsh.
- `internal/setup/service.go`: launchd commands (`launchctl bootstrap/bootout/kickstart/print`).
- `internal/tailscale`: the CLI may only exist inside the app bundle
  (`/Applications/Tailscale.app/Contents/MacOS/Tailscale`).
- Temp directories live under `/var/folders/...`, which is a symlink to `/private/var/...`;
  path comparisons in tests resolve symlinks for that reason.

## 2. Integration tests (real tmux on private sockets)

```sh
go test -race -tags integration -coverpkg=./internal/... -coverprofile=coverage.out ./...
scripts/check-coverage.sh coverage.out 90
```

Watch in particular:

- `internal/tmux`: the zombie-pane workaround in `ListPanes` (`run-shell true`) was found
  on tmux 3.4 on Linux; check that exit status 0 still closes the session on macOS tmux.
- `internal/server` `TestServerRestartKeepsAgents`: killing the server's process group
  must leave the agent alive.
- `internal/cli` `TestLauncherInsideTmuxSwitchesClient`: needs a PTY.

## 3. Browser test

```sh
npm --prefix e2e ci
npx --prefix e2e playwright install chromium
npm --prefix e2e test
```

## 4. Installer test

```sh
scripts/test-install.sh
```

It uses `sha256sum` to build the fake release; on macOS without coreutils, either
`brew install coreutils` or switch that line to `shasum -a 256`. `install.sh` itself
already falls back to `shasum`.

## 5. Install from source

```sh
./install.sh --source
```

Expected:

- `~/Library/LaunchAgents/com.nostromo.server.plist` exists, contains `AbandonProcessGroup`,
  and `launchctl print gui/$(id -u)/com.nostromo.server` shows it running.
- `~/.local/bin/agent` points to `~/.nostromo/bin/agent`; an `alias agent=...` in
  `~/.zshrc` is commented out with `# nostromo: disabled `.
- `tailscale serve status` proxies https 443 (or 8443) to `http://127.0.0.1:8787`.
- `agent doctor` is all green, or every non-green line has a fix that is about the
  environment (for example Tailscale settings), not about nostromo.

Report any "Left to do by hand" item from setup to the owner; do not run `sudo` without asking.

## 6. Manual acceptance

In a new terminal:

1. `agent` in a project directory opens Claude Code in tmux, with "bypass permissions on".
2. `Ctrl-b d`, then `agent ls` shows the chat as running.
3. Open the HTTPS address in Safari on the Mac and on a phone: the chat is listed, the
   terminal works, typing echoes, the phone key bar and input line work.
4. In the chat run `/clear`; `~/.nostromo/chats/<id>.json` gets a new `resume_id`.
5. `launchctl kickstart -k gui/$(id -u)/com.nostromo.server`: the chat keeps running
   (same agent PID before and after: `tmux -L nostromo list-panes -a -F '#{pane_pid}'`).
6. Log out and back in (or reboot): the service is back after login, the chat shows as
   stopped, Resume continues the conversation.
7. `agent uninstall`: the plist, the serve config, the hooks and the symlink are gone; the
   alias is restored. Then `./install.sh --source` again to leave the machine installed.

## 7. Results

| Step | Result | Notes |
| --- | --- | --- |
| 1. Unit tests | | |
| 2. Integration tests | | |
| 3. Browser test | | |
| 4. Installer test | | |
| 5. Install from source | | |
| 6. Manual acceptance | | |
