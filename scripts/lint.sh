#!/usr/bin/env bash
# Lint and format checks: one service (./scripts/lint.sh auth) or all of them.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/services.sh

run_one() {
  local s="$1" lang dir
  lang="$(service_lang "$s")"
  dir="$(service_dir "$s")"
  echo "==> lint $s ($lang)"
  case "$lang" in
    go)
      (cd "$dir" && go vet ./... && test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; })
      ;;
    python)
      (cd "$dir" && uv run --frozen ruff check . && uv run --frozen ruff format --check . && uv run --frozen mypy .)
      ;;
    java) (cd "$dir" && ./gradlew --quiet compileJava compileTestJava) ;;
    proto) ./scripts/proto.sh check ;;
    # actionlint checks workflow syntax, expressions and (with shellcheck installed) run scripts.
    ci) go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 ;;
    web) (cd "$dir" && { [[ -d node_modules ]] || npm ci; } && npm run lint) ;;
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
fi
