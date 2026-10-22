#!/usr/bin/env bash
# make explain (task 6.6): EXPLAIN (ANALYZE, BUFFERS) of the marketplace's and the assets
# service's queries against the local stack (seed it first: make seed). Each query runs three
# times; the table shows the median execution time and how the plan reads its tables. The full
# plans go to EXPLAIN_OUT (default: a temporary directory). Before and after numbers belong in
# docs/performance.md.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
env_file="${ENV_FILE:-.env}"
out="${EXPLAIN_OUT:-$(mktemp -d)}"
mkdir -p "$out"
for db in marketplace assets; do
  for run in 1 2 3; do
    docker compose -f infra/docker-compose.yml --env-file "$env_file" exec -T postgres \
      psql -X -q -U "$db" -d "$db" -f - < "scripts/explain/$db.sql" > "$out/$db-$run.txt"
  done
done
python3 - "$out" <<'PY'
import re, statistics, sys
from pathlib import Path

out = Path(sys.argv[1])
print(f"{'query':32} {'median ms':>10}  plan")
for db in ("marketplace", "assets"):
    times, plans = {}, {}
    for run in (1, 2, 3):
        name = None
        for line in (out / f"{db}-{run}.txt").read_text().splitlines():
            if line.startswith("### "):
                name = line[4:].strip()
                plans.setdefault(name, [])
            elif name and (m := re.search(r"Execution Time: ([\d.]+) ms", line)):
                times.setdefault(name, []).append(float(m.group(1)))
            elif name and run == 1 and (m := re.search(r"((?:Parallel )?(?:Seq Scan|Index Only Scan|Index Scan|Bitmap Heap Scan)(?: Backward)?(?: using \S+)? on \S+)", line)):
                if m.group(1) not in plans[name]:
                    plans[name].append(m.group(1))
    for name, ts in times.items():
        print(f"{db + '.' + name:32} {statistics.median(ts):>10.2f}  {'; '.join(plans.get(name, []))}")
print(f"\nfull plans: {out}")
PY
