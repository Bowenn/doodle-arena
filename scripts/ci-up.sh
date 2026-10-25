#!/usr/bin/env bash
# make up for CI's stack job, retried once, and only when it failed on a registry or a download:
# a Docker Hub 502, a rate limit, a timed-out pull or a body that arrived corrupted (58bb508,
# 2026-10-24: a 502, then a digest mismatch). Any other failure (a build error, a service that
# never gets healthy) fails at once, so a real bug is never retried away. scripts/ci-pull.sh pulls
# ahead with retries, but BuildKit still asks the registry for every base image's metadata during
# the build, so the build itself needs this.
#
#   PROFILES=live,analytics,search scripts/ci-up.sh
#   scripts/ci-up.sh --check-log <file>   exit 0 if the log shows a registry error (for tests)
set -euo pipefail

cd "$(dirname "$0")/.."

# Errors that mean "the registry or the network to it", never "our code".
REGISTRY_ERRORS='502 Bad Gateway|503 Service Unavailable|504 Gateway Time-?out|toomanyrequests|TLS handshake timeout|failed to (do request|fetch anonymous token|resolve source metadata).*(timeout|EOF|connection reset)|registry-1\.docker\.io.*(i/o timeout|connection reset)|unexpected EOF|digest mismatch'

registry_error() {
  grep -qE "$REGISTRY_ERRORS" "$1"
}

if [[ "${1:-}" == --check-log ]]; then
  registry_error "${2:?usage: scripts/ci-up.sh --check-log <file>}"
  exit
fi

log="$(mktemp)"
if make up 2>&1 | tee "$log"; then
  exit 0
fi
if ! registry_error "$log"; then
  echo "ci-up: make up failed, and not on a registry: no retry" >&2
  exit 1
fi
echo "ci-up: make up failed on a registry error ($(grep -oE "$REGISTRY_ERRORS" "$log" | head -1)); make down, then one more try" >&2
make down
make up
