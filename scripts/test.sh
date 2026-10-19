#!/usr/bin/env bash
# Run tests with the host toolchains: one service (./scripts/test.sh lobby) or all of them.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/services.sh

# Testcontainers needs the Docker CLI helpers (credential store) and the daemon socket. Docker
# Desktop keeps both under ~/.docker and may not create /var/run/docker.sock.
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
if [[ -z "${DOCKER_HOST:-}" ]] && command -v docker >/dev/null; then
  DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
  export DOCKER_HOST
  # Path of the socket inside Docker's VM, for Testcontainers' cleanup container.
  export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="${TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE:-/var/run/docker.sock}"
fi

run_one() {
  local s="$1" lang dir
  lang="$(service_lang "$s")"
  dir="$(service_dir "$s")"
  echo "==> test $s ($lang)"
  case "$lang" in
    go) (cd "$dir" && go test -race ./...) ;;
    python) (cd "$dir" && uv run --frozen pytest) ;;
    java) (cd "$dir" && ./gradlew --quiet test) ;;
    proto) ./scripts/proto.sh check ;;
    ci) echo "(no tests; workflows are checked by make lint s=ci)" ;;
    secrets) echo "(no tests; history is scanned by make lint s=secrets)" ;;
    observability) echo "(no tests; checked by make lint s=observability, and live by make alerts-check)" ;;
    terraform) echo "(no tests; checked by make lint s=terraform, and live by make aws-check)" ;;
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
