#!/usr/bin/env bash
# Removes nostromo from this machine: the background service, the tailscale serve
# config, the Claude Code hooks, the `agent` command and the rc-file edits. An
# `agent` alias that setup disabled is restored.
#
#   ./uninstall.sh               keep chat records in ~/.nostromo
#   ./uninstall.sh --purge       also delete ~/.nostromo
#   ./uninstall.sh --kill-chats  also end every running chat (tmux -L nostromo kill-server)
#
# It uses `agent uninstall` when the installed binary works and falls back to
# removing the same pieces itself when it does not.
set -euo pipefail

say() { printf '%s\n' "$*"; }
die() { printf 'uninstall.sh: %s\n' "$*" >&2; exit 1; }

home="${NOSTROMO_HOME:-$HOME/.nostromo}"
cmd_name="${NOSTROMO_CMD:-agent}"
link="$HOME/.local/bin/$cmd_name"
binary="$home/bin/agent"
socket="nostromo"

restore_rc() {
  local rc="$1" tmp
  [ -f "$rc" ] || return 0
  grep -q -e '^# nostromo: disabled ' -e '^# >>> nostromo >>>' "$rc" || return 0
  tmp="$(mktemp)"
  # Drops the marked block (and the blank line setup put before it) and
  # uncomments lines that setup disabled.
  awk '
    /^# >>> nostromo >>>$/ { skip = 1; blank = 0; next }
    /^# <<< nostromo <<<$/ { skip = 0; next }
    skip { next }
    /^$/ { if (blank) print ""; blank = 1; next }
    { if (blank) print ""; blank = 0; sub(/^# nostromo: disabled /, ""); print }
    END { if (blank) print "" }
  ' "$rc" > "$tmp"
  cat "$tmp" > "$rc"
  rm -f "$tmp"
  say "  restored $rc"
}

remove_hooks() {
  local settings="$HOME/.claude/settings.json"
  [ -f "$settings" ] && grep -q '/.nostromo/bin/agent hook' "$settings" || return 0
  if ! command -v python3 >/dev/null 2>&1; then
    say "  ! remove the nostromo hooks from $settings by hand (lines with /.nostromo/bin/agent hook)"
    return 0
  fi
  python3 - "$settings" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    data = json.load(f)
hooks = data.get("hooks", {})
for event in list(hooks):
    groups = []
    for group in hooks[event]:
        entries = [h for h in group.get("hooks", []) if "/.nostromo/bin/agent hook" not in h.get("command", "")]
        if entries:
            group["hooks"] = entries
            groups.append(group)
    if groups:
        hooks[event] = groups
    else:
        del hooks[event]
if not hooks:
    data.pop("hooks", None)
with open(path, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
PY
  say "  removed Claude Code hooks from $settings"
}

fallback() {
  say "The installed binary is missing or broken; removing the pieces directly."
  case "${NOSTROMO_SERVICE:-}:$(uname -s)" in
    none:*) ;;
    *:Linux)
      if command -v systemctl >/dev/null 2>&1; then
        systemctl --user disable --now nostromo >/dev/null 2>&1 || true
        rm -f "$HOME/.config/systemd/user/nostromo.service"
        systemctl --user daemon-reload >/dev/null 2>&1 || true
        say "  removed systemd service"
      fi
      if [ -f "$home/linger-enabled-by-setup" ]; then
        loginctl disable-linger "$(id -un)" >/dev/null 2>&1 && say "  turned linger off again (setup had turned it on)"
        rm -f "$home/linger-enabled-by-setup"
      fi
      ;;
    *:Darwin)
      launchctl bootout "gui/$(id -u)/com.nostromo.server" >/dev/null 2>&1 || true
      rm -f "$HOME/Library/LaunchAgents/com.nostromo.server.plist"
      say "  removed launchd agent"
      ;;
  esac
  if command -v tailscale >/dev/null 2>&1 && tailscale serve status 2>/dev/null | grep -q '127.0.0.1:8787'; then
    say "  ! tailscale serve still proxies to nostromo; turn it off with: tailscale serve --https=443 off (or 8443)"
  fi
  remove_hooks
  if [ -L "$link" ] && [ "$(readlink "$link")" = "$binary" ]; then
    rm -f "$link"
    say "  removed $link"
  fi
  for rc in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.config/fish/config.fish"; do
    restore_rc "$rc"
  done
}

main() {
  local purge=0 kill_chats=0
  for arg in "$@"; do
    case "$arg" in
      --purge) purge=1 ;;
      --kill-chats) kill_chats=1 ;;
      -h | --help) sed -n '2,12p' "$0"; exit 0 ;;
      *) die "unknown option: $arg" ;;
    esac
  done

  if [ "$kill_chats" = 1 ] && command -v tmux >/dev/null 2>&1; then
    tmux -L "$socket" kill-server >/dev/null 2>&1 || true
    say "Ended all running chats."
  fi

  if [ -x "$binary" ] && "$binary" version >/dev/null 2>&1; then
    if [ "$purge" = 1 ]; then
      "$binary" uninstall --purge
    else
      "$binary" uninstall
    fi
  else
    fallback
    if [ "$purge" = 1 ]; then
      rm -rf "$home"
      say "  removed $home"
    fi
  fi
  say ""
  say "Open a new terminal: \"$cmd_name\" is no longer the nostromo command there."
}

main "$@"
