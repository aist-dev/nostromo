#!/usr/bin/env bash
# Installs nostromo: downloads the agent binary for this machine from GitHub
# Releases, verifies its SHA-256 checksum and runs `agent setup`.
#
#   curl -fsSL https://raw.githubusercontent.com/aist-dev/nostromo/main/install.sh | bash
#
# Environment:
#   NOSTROMO_VERSION       release tag to install (default: latest)
#   NOSTROMO_HOME          install directory (default: ~/.nostromo)
#   NOSTROMO_CMD           command name on PATH (default: agent)
#   NOSTROMO_DOWNLOAD_URL  release base URL (default: https://github.com/aist-dev/nostromo/releases)
#   NOSTROMO_SETUP_ARGS    extra arguments for `agent setup`
set -euo pipefail

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *) die "unsupported operating system: $(uname -s) (macOS and Linux are supported)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    arm64 | aarch64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m) (amd64 and arm64 are supported)" ;;
  esac
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "need sha256sum or shasum to verify the download"
  fi
}

main() {
  local base="${NOSTROMO_DOWNLOAD_URL:-https://github.com/aist-dev/nostromo/releases}"
  local version="${NOSTROMO_VERSION:-latest}"
  local home="${NOSTROMO_HOME:-$HOME/.nostromo}"
  local os arch file url_dir tmp expected actual

  for tool in curl tar awk; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
  done
  if ! command -v tmux >/dev/null 2>&1; then
    say "Note: tmux is not installed. Install it (brew install tmux / sudo apt install tmux); setup checks it."
  fi

  os="$(detect_os)"
  arch="$(detect_arch)"
  file="agent_${os}_${arch}.tar.gz"
  if [ "$version" = "latest" ]; then
    url_dir="$base/latest/download"
  else
    case "$version" in v*) ;; *) version="v$version" ;; esac
    url_dir="$base/download/$version"
  fi

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  say "Downloading $file ($version)"
  curl -fsSL -o "$tmp/$file" "$url_dir/$file" || die "download failed: $url_dir/$file"
  curl -fsSL -o "$tmp/checksums.txt" "$url_dir/checksums.txt" || die "download failed: $url_dir/checksums.txt"

  expected="$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
  [ -n "$expected" ] || die "$file is not listed in checksums.txt"
  actual="$(sha256 "$tmp/$file")"
  [ "$expected" = "$actual" ] || die "checksum mismatch for $file (expected $expected, got $actual); nothing was changed"

  tar -xzf "$tmp/$file" -C "$tmp" agent || die "archive does not contain the agent binary"
  mkdir -p "$home/bin"
  cp "$tmp/agent" "$home/bin/agent.new"
  chmod 755 "$home/bin/agent.new"
  mv -f "$home/bin/agent.new" "$home/bin/agent"
  say "Installed $home/bin/agent ($("$home/bin/agent" version))"

  # shellcheck disable=SC2086
  NOSTROMO_HOME="$home" exec "$home/bin/agent" setup ${NOSTROMO_SETUP_ARGS:-}
}

main "$@"
