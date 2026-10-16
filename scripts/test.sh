#!/usr/bin/env bash
# Run tests with the host toolchains: one service (./scripts/test.sh lobby) or all of them.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/services.sh

run_one() {
  local s="$1" lang dir
  lang="$(service_lang "$s")"
  dir="$(service_dir "$s")"
  echo "==> test $s ($lang)"
  case "$lang" in
    go) (cd "$dir" && go test -race ./...) ;;
    python) (cd "$dir" && uv run --frozen pytest) ;;
    java) (cd "$dir" && ./gradlew --quiet test) ;;
    web) (cd "$dir" && { [[ -d node_modules ]] || npm ci; } && npm test) ;;
  esac
}

if [[ $# -gt 0 ]]; then
  run_one "$1"
else
  failed=()
  for s in "${ALL_SERVICES[@]}"; do
    run_one "$s" || failed+=("$s")
  done
  if [[ ${#failed[@]} -gt 0 ]]; then
    echo "FAILED: ${failed[*]}" >&2
    exit 1
  fi
  echo "All tests passed."
fi
