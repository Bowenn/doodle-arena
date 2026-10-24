"""Offline checks for dashboards and alerts as code (`make lint s=observability`, task 5.1).

- Dashboards (infra/observability/dashboards/*.json): valid JSON, unique uids and panel ids, only
  the provisioned data sources.
- Alert rules (infra/observability/grafana/alerting/rules.yaml): exactly the alerts in
  docs/observability.md section 5 (AWS ones aside), each with a severity, an existing runbook and
  an existing dashboard panel.
- Runbooks (docs/runbooks/*.md): the template's sections, and every alert that links to one is
  named in it.
- Every metric a panel or rule queries is one docs/observability.md lists, so a renamed metric
  can't leave a dashboard silently empty.

Run with uv (PyYAML is the only dependency):
  uv run --no-project --with pyyaml==6.0.3 python scripts/observability-lint.py
"""

import json
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
DASHBOARDS = ROOT / "infra/observability/dashboards"
ALERTING = ROOT / "infra/observability/grafana/alerting"
RUNBOOKS = ROOT / "docs/runbooks"
SPEC = ROOT / "docs/observability.md"
RUNBOOK_URL = "https://github.com/Bowenn/doodle-arena/blob/master/docs/runbooks/"

# Alerts in the spec that aren't Grafana rules: AWS Budgets / CloudWatch (task 5.2).
NOT_GRAFANA = {"AwsBudget"}
# Runbooks for failures a script reports instead of an alert: aws-down's backup and aws-up's
# restore (task 6.10) exit with BACKUP FAILED or RESTORE FAILED.
SCRIPT_RUNBOOKS = {"aws-backup.md"}
# Metrics from instrumentation, the kafka-metrics collector or k6 rather than the spec's table
# (observability.md lists each source).
STANDARD = ("http_server_", "rpc_server_", "rpc_client_", "kafka_consumer_group_", "target_info", "go_", "k6_")
PROMQL_WORDS = {
    "sum", "max", "min", "avg", "count", "abs", "rate", "increase", "delta", "deriv", "by", "without",
    "on", "ignoring", "and", "or", "unless", "vector", "histogram_quantile", "last_over_time",
    "max_over_time", "absent_over_time", "offset", "group_left", "group_right", "clamp_max", "le",
    "label_replace", "time", "bool",
}
# Flink's metric reporter (ADR 0018) prefixes every metric with its scope, for example
# flink_taskmanager_job_task_operator_rule_analytics_wash_trades_flagged_total. The job's own
# counters are checked under their analytics.* names; the rest are Flink's own, like the
# instrumentation above.
FLINK_JOB_METRIC = re.compile(r"^flink_[a-z_]*?_(analytics_[a-z0-9_]+)$")
RUNBOOK_SECTIONS = ("## What it means", "## Check", "## Fix", "## Afterwards")

errors: list[str] = []


def fail(msg: str) -> None:
    errors.append(msg)


def spec_metrics() -> set[str]:
    """Metric names from the spec's table, as Prometheus spells them before unit suffixes."""
    names = set()
    for m in re.finditer(r"^\| `([a-z0-9_.]+)`(?:, `([a-z0-9_.]+)`)*", SPEC.read_text(), re.M):
        for n in re.findall(r"`([a-z0-9_.]+)`", m.group(0)):
            names.add(n.replace(".", "_"))
    return names


def spec_alerts() -> set[str]:
    section = SPEC.read_text().split("## 5. Alerting", 1)[1].split("\n## ", 1)[0]
    return set(re.findall(r"^\| ([A-Z][A-Za-z]+) \|", section, re.M)) - {"Alert"}


def metric_names(expr: str) -> set[str]:
    """The metric names a PromQL expression reads (label matchers, groupings and strings removed)."""
    expr = re.sub(r'"[^"]*"', "", expr)
    expr = re.sub(r"\{[^}]*\}", "", expr)
    expr = re.sub(r"\b(by|without|on|ignoring)\s*\([^)]*\)", "", expr)
    expr = re.sub(r"\[[^\]]*\]", "", expr)
    return {w for w in re.findall(r"\b[a-z_][a-z0-9_]*\b", expr) if w not in PROMQL_WORDS}


def known(metric: str, spec: set[str]) -> bool:
    if metric.startswith(STANDARD):
        return True
    flink = FLINK_JOB_METRIC.match(metric)
    if flink:
        metric = flink.group(1)
    elif metric.startswith("flink_"):
        return True
    base = re.sub(r"_(bucket|count|sum)$", "", metric)
    base = re.sub(r"_total$", "", base)
    # A name that already ends in its unit (game.ws.bytes) gets no second suffix.
    if base in spec:
        return True
    base = re.sub(r"_(seconds|milliseconds|microseconds|bytes)$", "", base)
    # A rate unit such as {request}/min becomes a _per_min suffix in Prometheus.
    base = re.sub(r"_per_(min|minute|second)$", "", base)
    return base in spec


def check_dashboards(spec: set[str]) -> dict[str, set[int]]:
    panels_by_uid: dict[str, set[int]] = {}
    for path in sorted(DASHBOARDS.glob("*.json")):
        name = path.name
        try:
            d = json.loads(path.read_text())
        except ValueError as e:
            fail(f"{name}: invalid JSON: {e}")
            continue
        uid = d.get("uid")
        if not uid or uid in panels_by_uid:
            fail(f"{name}: missing or duplicate uid {uid!r}")
            continue
        ids: set[int] = set()
        for p in d.get("panels", []):
            if p["id"] in ids:
                fail(f"{name}: duplicate panel id {p['id']}")
            ids.add(p["id"])
            for t in p.get("targets", []):
                ds = t.get("datasource", {}).get("uid")
                if ds not in ("prometheus", "loki", "cloudwatch"):
                    fail(f"{name} panel {p['id']}: unknown data source {ds!r}")
                if ds == "prometheus":
                    for m in metric_names(t["expr"]):
                        if not known(m, spec):
                            fail(f"{name} panel {p['id']}: metric {m} isn't in docs/observability.md")
        panels_by_uid[uid] = ids
    if not panels_by_uid:
        fail("no dashboards found")
    return panels_by_uid


def check_rules(spec: set[str], panels: dict[str, set[int]]) -> dict[str, set[str]]:
    """Returns the alert titles that link to each runbook file."""
    by_runbook: dict[str, set[str]] = {}
    doc = yaml.safe_load((ALERTING / "rules.yaml").read_text())
    titles, uids = set(), set()
    for group in doc["groups"]:
        for r in group["rules"]:
            title, uid = r["title"], r["uid"]
            if title in titles or uid in uids:
                fail(f"rule {title}: duplicate title or uid")
            titles.add(title)
            uids.add(uid)
            if r["labels"].get("severity") not in ("critical", "warning"):
                fail(f"rule {title}: severity must be critical or warning")
            ann = r["annotations"]
            url = ann.get("runbook_url", "")
            if not url.startswith(RUNBOOK_URL) or not (RUNBOOKS / url.removeprefix(RUNBOOK_URL)).is_file():
                fail(f"rule {title}: runbook {url!r} doesn't exist in docs/runbooks/")
            else:
                by_runbook.setdefault(url.removeprefix(RUNBOOK_URL), set()).add(title)
            dash, panel = ann.get("__dashboardUid__"), ann.get("__panelId__", "")
            if dash not in panels or not panel.isdigit() or int(panel) not in panels[dash]:
                fail(f"rule {title}: dashboard panel {dash}/{panel} doesn't exist")
            if not ann.get("summary"):
                fail(f"rule {title}: no summary")
            if r["condition"] not in {d["refId"] for d in r["data"]}:
                fail(f"rule {title}: condition {r['condition']} isn't one of its queries")
            for d in r["data"]:
                if d["datasourceUid"] == "prometheus":
                    for m in metric_names(d["model"]["expr"]):
                        if not known(m, spec):
                            fail(f"rule {title}: metric {m} isn't in docs/observability.md")
    want = spec_alerts() - NOT_GRAFANA
    for missing in sorted(want - titles):
        fail(f"alert {missing} is in docs/observability.md but has no rule")
    for extra in sorted(titles - want):
        fail(f"rule {extra} isn't in docs/observability.md section 5")
    return by_runbook


def check_runbooks(by_runbook: dict[str, set[str]]) -> None:
    for path in sorted(RUNBOOKS.glob("*.md")):
        if path.name in ("TEMPLATE.md", "README.md"):  # the template, and the AWS commands page
            continue
        text = path.read_text()
        for section in RUNBOOK_SECTIONS:
            if section not in text:
                fail(f"runbook {path.name}: missing section {section!r}")
        for title in by_runbook.get(path.name, ()):
            if title not in text:
                fail(f"runbook {path.name}: doesn't mention {title}, which links to it")
        if path.name not in by_runbook and path.name not in SCRIPT_RUNBOOKS:
            fail(f"runbook {path.name}: no alert links to it")


def main() -> int:
    spec = spec_metrics()
    panels = check_dashboards(spec)
    by_runbook = check_rules(spec, panels)
    check_runbooks(by_runbook)
    for e in errors:
        print(f"observability-lint: {e}", file=sys.stderr)
    if not errors:
        print(f"observability-lint: {len(panels)} dashboards, {sum(len(t) for t in by_runbook.values())} rules, "
              f"{len(by_runbook)} runbooks OK")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
