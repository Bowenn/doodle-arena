#!/usr/bin/env bash
# Validates the collector overlays (infra/observability/otelcol/*.yaml) merged in the order both
# composes load them, over a stand-in for otel-lgtm's own config, with the contrib collector of
# the same version (0.162.0, as in otel-lgtm 0.36.0). An invalid overlay stops otel-lgtm's
# collector from starting, and with it every trace, metric and log (task 6.9). Part of
# make lint s=observability; skipped without Docker.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
if ! docker info >/dev/null 2>&1; then
  echo "otelcol-validate: skipped (no Docker)"
  exit 0
fi
dir=infra/observability/otelcol
docker run --rm -e DEPLOYMENT_ENVIRONMENT=local -v "$PWD/$dir:/x:ro" otel/opentelemetry-collector-contrib:0.162.0 \
  validate --config=file:/x/testdata/otel-lgtm-base.yaml --config=file:/x/memory-limiter.yaml \
  --config=file:/x/tail-sampling.yaml --config=file:/x/browser.yaml
echo "otelcol-validate: the collector overlays are valid"
