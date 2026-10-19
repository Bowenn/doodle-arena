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
    # gitleaks scans every commit of the checked-out history (rules and exceptions: .gitleaks.toml).
    secrets) go run github.com/zricethezav/gitleaks/v8@v8.28.0 git --redact --no-banner . ;;
    web) (cd "$dir" && { [[ -d node_modules ]] || npm ci; } && npm run lint) ;;
    # Dashboards, alert rules and runbooks against each other and docs/observability.md.
    observability) uv run --no-project --with pyyaml==6.0.3 python scripts/observability-lint.py ;;
    # Formatting, both roots and the module validated offline (no AWS access), then tflint.
    terraform)
      (cd "$dir" && terraform fmt -check -recursive \
        && for root in bootstrap stack modules/service; do
          terraform -chdir="$root" init -backend=false -input=false >/dev/null && terraform -chdir="$root" validate -no-color || exit 1
        done \
        && go run github.com/terraform-linters/tflint@v0.64.0 --init >/dev/null \
        && go run github.com/terraform-linters/tflint@v0.64.0 --recursive --config "$PWD/.tflint.hcl")
      ;;
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
