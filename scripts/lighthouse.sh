#!/usr/bin/env bash
# Lighthouse (task 5.4, web/lighthouserc.json): three runs of the lobby and the gallery with
# Lighthouse's default mobile emulation (a mid-range phone on a slow 4G link), asserting the
# median run: performance score >= 0.9, LCP <= 2.5 s, CLS <= 0.1 (TBT and accessibility warn).
#   scripts/lighthouse.sh                                 the production web image, run locally
#   GATEWAY=https://icanfly.click scripts/lighthouse.sh   the AWS stack (make aws-up)
# The local run builds web/Dockerfile's static target (the image AWS runs) and serves it alone,
# so it measures the bundle and nginx's caching and compression; the app's API calls fail there,
# so only AWS gives the real numbers. Reports stay on disk (web/.lighthouseci/reports), never
# on public storage. It needs Google Chrome; the lhci CLI comes from npm, pinned.
set -euo pipefail

cd "$(dirname "$0")/.."
LHCI_VERSION=0.15.1
PAGES=(/ /gallery)

container=""
cleanup() { if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null; fi; }
trap cleanup EXIT

if [[ -n "${GATEWAY:-}" ]]; then
  base="${GATEWAY%/}"
else
  docker build -q --target static -t doodle-arena-web-static web >/dev/null
  container="$(docker run -d --rm -p 127.0.0.1::8080 doodle-arena-web-static)"
  base="http://$(docker port "$container" 8080/tcp | head -1)"
  for _ in $(seq 1 20); do curl -fs -o /dev/null "$base/" && break; sleep 0.5; done
fi

urls=()
for page in "${PAGES[@]}"; do urls+=("--collect.url=$base$page"); done
rm -rf web/.lighthouseci
status=0
(cd web && npx -y "@lhci/cli@$LHCI_VERSION" autorun --config=lighthouserc.json "${urls[@]}") || status=$?

# The median run of each page, for the evidence log (docs/performance.md).
python3 - web/.lighthouseci/reports <<'EOF'
import json, pathlib, statistics, sys
reports = pathlib.Path(sys.argv[1])
runs = {}
for entry in json.loads((reports / "manifest.json").read_text()):
    runs.setdefault(entry["url"], []).append(json.loads(pathlib.Path(entry["jsonPath"]).read_text()))
print(f"{'page':<40} {'perf':>5} {'LCP ms':>7} {'CLS':>6} {'TBT ms':>7} {'FCP ms':>7} {'a11y':>5}")
for url, reps in runs.items():
    med = lambda f: statistics.median(f(r) for r in reps)
    audit = lambda name: (lambda r: r["audits"][name]["numericValue"])
    print(f"{url:<40} {med(lambda r: r['categories']['performance']['score']):>5.2f}"
          f" {med(audit('largest-contentful-paint')):>7.0f} {med(audit('cumulative-layout-shift')):>6.3f}"
          f" {med(audit('total-blocking-time')):>7.0f} {med(audit('first-contentful-paint')):>7.0f}"
          f" {med(lambda r: r['categories']['accessibility']['score']):>5.2f}")
print(f"reports: {reports}")
EOF
exit "$status"
