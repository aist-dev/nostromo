#!/usr/bin/env bash
# Tests install.sh against a local HTTP server that mimics GitHub Releases:
# a good release installs, a tampered release is refused.
#
#   scripts/test-install.sh             checks run in a throwaway HOME on this machine
#   scripts/test-install.sh --container checks run as a fresh user in a clean Ubuntu container (needs docker)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
version=0.0.7

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

make_release() {
  # make_release <dir> <goos>: builds a release for <goos> and the host architecture into <dir>.
  local dir="$1" goos="$2" goarch work
  goarch="$(go env GOARCH)"
  work="$(mktemp -d)"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -C "$root" \
    -ldflags "-X github.com/aist-dev/nostromo/internal/version.Version=$version" -o "$work/agent" ./cmd/agent
  mkdir -p "$dir/download/v$version" "$dir/latest/download"
  tar -czf "$dir/download/v$version/agent_${goos}_${goarch}.tar.gz" -C "$work" agent
  (cd "$dir/download/v$version" && sha256sum ./*.tar.gz | sed 's# \./# #' > checksums.txt)
  cp "$dir/download/v$version/"* "$dir/latest/download/"
  rm -rf "$work"
}

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

wait_port() {
  for _ in $(seq 1 50); do
    curl -fsS -o /dev/null "http://127.0.0.1:$1/" 2>/dev/null && return 0
    sleep 0.1
  done
  fail "http server on port $1 did not start"
}

# checks <good-url> <bad-url> <home>: the assertions; needs only bash, curl, tar, tmux.
checks() {
  local good="$1" bad="$2" home="$3" out
  out="$(mktemp)"
  run() {
    env -i HOME="$home" PATH="/usr/local/bin:/usr/bin:/bin" SHELL=/bin/bash USER="$(id -un)" \
      NOSTROMO_SETUP_ARGS="--no-service --no-tailscale" "$@" bash "$root/install.sh" > "$out" 2>&1
  }
  mkdir -p "$home"
  printf "alias agent='claude --dangerously-skip-permissions'\n" > "$home/.bashrc"

  run NOSTROMO_DOWNLOAD_URL="$good" || { cat "$out"; fail "install failed"; }
  [ "$("$home/.nostromo/bin/agent" version)" = "$version" ] || fail "wrong version installed"
  [ "$(readlink "$home/.local/bin/agent")" = "$home/.nostromo/bin/agent" ] || fail "symlink missing"
  grep -q '^# nostromo: disabled alias agent=' "$home/.bashrc" || fail "alias not disabled"
  [ -f "$home/.nostromo/config.toml" ] || fail "config missing"
  grep -q 'hook claude' "$home/.claude/settings.json" || fail "hooks missing"
  echo "ok: install from latest"

  run NOSTROMO_DOWNLOAD_URL="$good" NOSTROMO_VERSION="$version" || { cat "$out"; fail "pinned install failed"; }
  [ "$(grep -c 'nostromo: disabled' "$home/.bashrc")" = 1 ] || fail "second run edited the rc file again"
  echo "ok: pinned version, idempotent rerun"

  if run NOSTROMO_DOWNLOAD_URL="$bad"; then fail "tampered archive was accepted"; fi
  grep -q 'checksum mismatch' "$out" || { cat "$out"; fail "no checksum error"; }
  [ "$("$home/.nostromo/bin/agent" version)" = "$version" ] || fail "binary changed after a refused install"
  echo "ok: checksum mismatch refused"

  if run NOSTROMO_DOWNLOAD_URL="$good" NOSTROMO_VERSION=9.9.9; then fail "missing version was accepted"; fi
  echo "ok: missing release refused"
  rm -f "$out"
}

# source_check <home>: ./install.sh --source builds the checkout and installs it.
source_check() {
  local home="$1" out
  out="$(mktemp)"
  mkdir -p "$home"
  printf "alias agent='claude --dangerously-skip-permissions'\n" > "$home/.bashrc"
  env -i HOME="$home" PATH="$PATH" SHELL=/bin/bash USER="$(id -un)" GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" \
    NOSTROMO_SETUP_ARGS="--no-service --no-tailscale" bash "$root/install.sh" --source > "$out" 2>&1 || { cat "$out"; fail "source install failed"; }
  [ -x "$home/.nostromo/bin/agent" ] || fail "source build not installed"
  [ "$(readlink "$home/.local/bin/agent")" = "$home/.nostromo/bin/agent" ] || fail "symlink missing after source install"
  grep -q '^# nostromo: disabled alias agent=' "$home/.bashrc" || fail "alias not disabled by source install"
  rm -f "$out"
  echo "ok: install from source"
}

main() {
  if [ "${1:-}" = "--inside" ]; then
    checks "$2" "$3" "$HOME/install-test"
    return
  fi
  local container=0 goos good_port bad_port
  [ "${1:-}" = "--container" ] && container=1
  goos="$(go env GOOS)"
  [ "$container" = 1 ] && goos=linux

  tmp="$(mktemp -d)"
  pids=()
  trap 'kill "${pids[@]}" 2>/dev/null || true; rm -rf "${tmp:-}"' EXIT
  make_release "$tmp/good" "$goos"
  cp -r "$tmp/good" "$tmp/bad"
  printf 'garbage' >> "$tmp/bad/latest/download/agent_${goos}_$(go env GOARCH).tar.gz"
  good_port="$(free_port)"
  bad_port="$(free_port)"
  python3 -m http.server "$good_port" --bind 127.0.0.1 --directory "$tmp/good" >/dev/null 2>&1 & pids+=($!)
  python3 -m http.server "$bad_port" --bind 127.0.0.1 --directory "$tmp/bad" >/dev/null 2>&1 & pids+=($!)
  wait_port "$good_port"
  wait_port "$bad_port"
  local good="http://127.0.0.1:$good_port" bad="http://127.0.0.1:$bad_port"

  if [ "$container" = 0 ]; then
    checks "$good" "$bad" "$tmp/home"
    source_check "$tmp/source-home"
    return
  fi
  command -v docker >/dev/null || fail "docker is required for --container"
  docker run --rm --network host -v "$root:/src:ro" ubuntu:24.04 bash -euc '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq >/dev/null && apt-get install -y -qq tmux curl ca-certificates >/dev/null
    useradd -m tester
    su tester -c "/src/scripts/test-install.sh --inside $0 $1"
  ' "$good" "$bad"
}

main "$@"
