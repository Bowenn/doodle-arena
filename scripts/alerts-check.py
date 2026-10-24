"""Live check of dashboards and alerts as code against a running stack (`make alerts-check`).

After `make up`, asks Grafana whether it loaded what infra/observability/ provisions:
  - every dashboard exists, and every one of its queries runs (Prometheus and Loki);
  - every alert rule is loaded, has been evaluated, and evaluates without an error;
  - the Discord contact point exists and the notification policy sends to it.
Rules evaluate once a minute, so it waits up to TIMEOUT seconds (default 150) for the first
evaluation. Standard library only.
"""

# Annotations stay text: macOS ships Python 3.9, which can't evaluate `str | None`.
from __future__ import annotations

import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GRAFANA = os.environ.get("GRAFANA", "http://localhost:3000")
TIMEOUT = int(os.environ.get("TIMEOUT", "150"))
NEVER = "0001-01-01T00:00:00Z"


# In AWS, anonymous visitors only view; the provisioning API needs the admin (scripts/aws/check.sh).
AUTH = (
    {"Authorization": "Basic " + base64.b64encode(f"{os.environ['GRAFANA_USER']}:{os.environ['GRAFANA_PASSWORD']}".encode()).decode()}
    if os.environ.get("GRAFANA_USER")
    else {}
)


def get(path: str, **params: str) -> dict:
    url = f"{GRAFANA}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    with urllib.request.urlopen(urllib.request.Request(url, headers=AUTH), timeout=10) as resp:
        return json.load(resp)


def query_error(datasource: str, expr: str) -> str | None:
    """None when the query runs; otherwise the data source's error."""
    # Grafana's interval variables: $__rate_interval for Prometheus, $__auto for Loki.
    for var in ("$__rate_interval", "$__auto", "$__interval"):
        expr = expr.replace(var, "1m")
    try:
        if datasource == "prometheus":
            body = get("/api/datasources/proxy/uid/prometheus/api/v1/query", query=expr)
        else:
            body = get("/api/datasources/proxy/uid/loki/loki/api/v1/query_range", query=expr, since="15m", limit="1")
    except urllib.error.HTTPError as e:
        return f"HTTP {e.code}: {e.read().decode(errors='replace')[:200]}"
    return None if body.get("status") == "success" else str(body)[:200]


def check_dashboards() -> list[str]:
    problems = []
    for path in sorted((ROOT / "infra/observability/dashboards").glob("*.json")):
        d = json.loads(path.read_text())
        try:
            get(f"/api/dashboards/uid/{d['uid']}")
        except urllib.error.HTTPError as e:
            problems.append(f"dashboard {d['uid']} isn't loaded (HTTP {e.code})")
            continue
        queries = skipped = 0
        for p in d["panels"]:
            for t in p.get("targets", []):
                # CloudWatch panels (AWS only) go through Grafana's own query API, not a proxy.
                if t["datasource"]["uid"] not in ("prometheus", "loki"):
                    skipped += 1
                    continue
                queries += 1
                if err := query_error(t["datasource"]["uid"], t["expr"]):
                    problems.append(f"dashboard {d['uid']} panel {p['id']} ({p['title']}): {err}")
        print(f"dashboard {d['uid']}: loaded, {queries} queries run" + (f", {skipped} CloudWatch skipped" if skipped else ""))
    return problems


def expected_rules() -> set[str]:
    text = (ROOT / "infra/observability/grafana/alerting/rules.yaml").read_text()
    return set(re.findall(r"^\s+title: (\S+)$", text, re.M))


def rule_states() -> dict[str, dict]:
    groups = get("/api/prometheus/grafana/api/v1/rules")["data"]["groups"]
    return {r["name"]: r for g in groups for r in g["rules"]}


def check_rules() -> list[str]:
    want = expected_rules()
    deadline = time.monotonic() + TIMEOUT
    while True:
        states = rule_states()
        evaluated = {n for n, r in states.items() if r.get("lastEvaluation", NEVER) != NEVER}
        if want <= evaluated or time.monotonic() > deadline:
            break
        time.sleep(5)
    problems = [f"rule {n} isn't loaded" for n in sorted(want - states.keys())]
    problems += [f"rule {n} hasn't been evaluated within {TIMEOUT} s" for n in sorted((want & states.keys()) - evaluated)]
    for name in sorted(want & evaluated):
        r = states[name]
        if r["health"] != "ok":
            problems.append(f"rule {name}: {r['health']}: {r.get('lastError', '')[:200]}")
    firing = sorted(n for n in want & evaluated if states[n]["state"] == "firing")
    print(f"rules: {len(want & evaluated)}/{len(want)} evaluated" + (f"; firing now: {', '.join(firing)}" if firing else ""))
    return problems


def check_contact_point() -> list[str]:
    points = get("/api/v1/provisioning/contact-points")
    if not any(p["name"] == "discord" and p["type"] == "discord" for p in points):
        return ["contact point discord isn't loaded"]
    if get("/api/v1/provisioning/policies").get("receiver") != "discord":
        return ["the notification policy doesn't send to discord"]
    print("contact point discord: loaded, the policy sends to it")
    return []


def main() -> int:
    problems = check_contact_point() + check_dashboards() + check_rules()
    for p in problems:
        print(f"FAIL {p}", file=sys.stderr)
    if problems:
        return 1
    print("alerts-check: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
