#!/usr/bin/env bash
# Fails when total statement coverage in a Go cover profile is below a threshold.
# Usage: scripts/check-coverage.sh <coverprofile> [threshold-percent]
set -euo pipefail

main() {
  local profile="${1:?usage: check-coverage.sh <coverprofile> [threshold]}"
  local threshold="${2:-90}"
  local total
  total="$(go tool cover -func="$profile" | awk '/^total:/ { sub("%", "", $3); print $3 }')"
  if [[ -z "$total" ]]; then
    echo "could not read total coverage from $profile" >&2
    exit 1
  fi
  echo "total coverage: ${total}% (threshold ${threshold}%)"
  if awk -v t="$total" -v min="$threshold" 'BEGIN { exit !(t + 0 < min + 0) }'; then
    echo "coverage below threshold" >&2
    exit 1
  fi
}

main "$@"
