#!/usr/bin/env bash
# Contracts: lint proto/, regenerate code for Go, Java and Python, and check that it builds.
#   ./scripts/proto.sh           lint, regenerate, check   (make proto)
#   ./scripts/proto.sh check     lint and check, no regeneration (make test s=proto)
set -euo pipefail

cd "$(dirname "$0")/../proto"
mode="${1:-generate}"

echo "==> buf format and lint"
buf format --diff --exit-code
buf lint

if [[ "$mode" == generate ]]; then
  echo "==> buf generate"
  # Delete only generated code; the hand-written build files in gen/ stay.
  rm -rf gen/go/doodle gen/java/src gen/python/doodle
  buf generate
  (cd gen/go && go mod tidy)
fi

echo "==> check Go"
(cd gen/go && go build ./... && go vet ./...)

echo "==> check Java"
../services/marketplace/gradlew --quiet -p gen/java build

echo "==> check Python"
(cd gen/python && uv run --frozen pytest -q && uv run --frozen ruff check . && uv run --frozen ruff format --check .)

echo "Contracts OK."
