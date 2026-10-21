#!/usr/bin/env python3
"""The logic behind aws-down's backup and aws-up's restore (ADR 0016), kept out of the shell
scripts so it can be tested (scripts/aws/test_backups.py, run by make test s=terraform).
Standard library only. The shell scripts make every AWS call; this file only reads and writes
JSON and asks git.

  backups.py summary <command output>          the host's JSON summary, from between its markers
  backups.py manifest <summary> <id> <tag> <drawings objects> <drawings bytes>
  backups.py artifacts <manifest or summary>   "name file bytes sha256-hex sha256-base64" lines
  backups.py check-schema <manifest> <repo> <tag>
  backups.py compare-counts <manifest> <restore summary>
  backups.py metrics <direction> <result> <summary> [drawings seconds] [drawings bytes] [total seconds]
  backups.py log-events <direction> <result> <backup id> <version> <summary> [message]
"""

import base64
import json
import re
import subprocess
import sys
import time

MARKER_BEGIN = "BACKUP-SUMMARY-BEGIN"
MARKER_END = "BACKUP-SUMMARY-END"
# What a backup holds, in the order a restore loads it: MongoDB first, so that a restore that
# fails partway leaves data next to an empty database, which a rerun refuses (load.sh).
STORES = ["mongo-assets", "pg-auth", "pg-assets", "pg-marketplace", "pg-notifier"]
NAMESPACE = "DoodleArena/Backups"


def summary_from_output(text):
    """The JSON object a host script printed between its marker lines."""
    if MARKER_BEGIN not in text or MARKER_END not in text:
        raise ValueError("the command's output has no backup summary")
    body = text.split(MARKER_BEGIN, 1)[1].split(MARKER_END, 1)[0]
    return json.loads(body)


def build_manifest(summary, backup_id, image_tag, drawings_objects, drawings_bytes, now=None):
    names = sorted(a["name"] for a in summary["artifacts"])
    if names != sorted(STORES):
        raise ValueError(f"the backup has {names}, not {sorted(STORES)}")
    return {
        "id": backup_id,
        "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
        "image_tag": image_tag,
        "artifacts": sorted(summary["artifacts"], key=lambda a: STORES.index(a["name"])),
        "drawings": {"prefix": f"backups/{backup_id}/drawings/", "objects": drawings_objects, "bytes": drawings_bytes},
        "counts": summary["counts"],
        "migrations": summary["migrations"],
    }


def b64_of_hex(hex_digest):
    """S3 reports a SHA-256 checksum in base64; the host prints it in hex."""
    return base64.b64encode(bytes.fromhex(hex_digest)).decode()


# ---------------------------------------------------------------- schema check

def _git(repo, *args):
    return subprocess.run(["git", "-C", repo, *args], check=True, capture_output=True, text=True).stdout


def code_migrations(repo, tag):
    """Every migration the code at <tag> knows, per database, as the backup records them."""
    files = _git(repo, "ls-tree", "-r", "--name-only", tag, "--",
                 "services/auth/migrations/versions", "services/assets",
                 "services/marketplace/src/main/resources/db/migration",
                 "services/notifier/internal/store/migrations").splitlines()
    known = {"auth": set(), "assets": {}, "marketplace": set(), "notifier": set()}
    for path in files:
        name = path.rsplit("/", 1)[-1]
        if path.startswith("services/auth/migrations/versions/") and name.endswith(".py"):
            m = re.search(r'^revision(?:: str)? = ["\']([^"\']+)["\']', _git(repo, "show", f"{tag}:{path}"), re.M)
            if m:
                known["auth"].add(m.group(1))
        elif path.startswith("services/marketplace/") and (m := re.match(r"V(\d+)__.*\.sql$", name)):
            known["marketplace"].add(m.group(1))
        elif path.startswith("services/notifier/") and (m := re.match(r"0*(\d+)_.*\.up\.sql$", name)):
            known["notifier"].add(m.group(1))
        elif (m := re.match(r"services/assets/(\w+)/migrations/(\d{4}\w*)\.py$", path)):
            known["assets"].setdefault(m.group(1), set()).add(m.group(2))
    return known


def schema_problems(manifest, known):
    """Why the backup's schema is newer than the code's: a migration the backup has applied that
    the code doesn't have. Empty when the code can take the backup, and its own migrations then
    apply whatever is newer. Django's own apps (auth, admin, sessions...) come with the Django
    version, not the repo, so only assets' apps in the repo are checked."""
    problems = []
    applied = manifest["migrations"]
    for version in applied.get("auth", []):
        if version not in known["auth"]:
            problems.append(f"auth: alembic revision {version} isn't in the code")
    for version in applied.get("marketplace", []):
        if version not in known["marketplace"]:
            problems.append(f"marketplace: Flyway V{version} isn't in the code")
    for entry in applied.get("notifier", []):
        version, _, dirty = entry.partition(" ")
        if dirty:
            problems.append(f"notifier: migration {version} is dirty (it failed halfway)")
        elif version not in known["notifier"]:
            problems.append(f"notifier: migration {version} isn't in the code")
    for entry in applied.get("assets", []):
        app, _, name = entry.partition(".")
        if app in known["assets"] and name not in known["assets"][app]:
            problems.append(f"assets: Django migration {entry} isn't in the code")
    return problems


# ---------------------------------------------------------------- restore check

def count_problems(manifest, restored):
    """Rows or documents that differ between the backup and the restored stores."""
    problems = []
    for store, expected in manifest["counts"].items():
        got = restored["counts"].get(store, {})
        for table in sorted(set(expected) | set(got)):
            if expected.get(table) != got.get(table):
                problems.append(f"{store} {table}: {expected.get(table)} in the backup, {got.get(table)} restored")
    return problems


# ---------------------------------------------------------------- telemetry

def _done(summary):
    return {a["name"]: a for a in summary.get("artifacts", [])}


def metric_data(direction, result, summary, drawings_seconds=None, drawings_bytes=None, total_seconds=None):
    """put-metric-data's MetricData (docs/observability.md, "Backups"): duration and size per
    store, and one run per store with its result. A failed run counts the stores it finished as
    ok, the one it was on as error, and doesn't count the stores it never reached."""
    done = _done(summary)
    data = []

    def dims(**kv):
        return [{"Name": k, "Value": v} for k, v in kv.items()]

    for store in STORES:
        if store in done:
            a = done[store]
            data.append({"MetricName": "backup.duration", "Unit": "Seconds", "Value": a["seconds"], "Dimensions": dims(direction=direction, store=store)})
            data.append({"MetricName": "backup.size", "Unit": "Bytes", "Value": a["bytes"], "Dimensions": dims(direction=direction, store=store)})
            data.append({"MetricName": "backup.runs", "Unit": "Count", "Value": 1, "Dimensions": dims(direction=direction, store=store, result="ok")})
        elif result != "ok":
            data.append({"MetricName": "backup.runs", "Unit": "Count", "Value": 1, "Dimensions": dims(direction=direction, store=store, result="error")})
            break
    if drawings_seconds is not None:
        data.append({"MetricName": "backup.duration", "Unit": "Seconds", "Value": drawings_seconds, "Dimensions": dims(direction=direction, store="drawings")})
        if drawings_bytes is not None:
            data.append({"MetricName": "backup.size", "Unit": "Bytes", "Value": drawings_bytes, "Dimensions": dims(direction=direction, store="drawings")})
        data.append({"MetricName": "backup.runs", "Unit": "Count", "Value": 1, "Dimensions": dims(direction=direction, store="drawings", result=result)})
    if total_seconds is not None:
        data.append({"MetricName": "backup.duration", "Unit": "Seconds", "Value": total_seconds, "Dimensions": dims(direction=direction, store="all")})
    data.append({"MetricName": "backup.runs", "Unit": "Count", "Value": 1, "Dimensions": dims(direction=direction, store="all", result=result)})
    return data


def log_events(direction, result, backup_id, version, summary, message="", now=None):
    """put-log-events' events: JSON lines in the services' log format (CLAUDE.md, "Telemetry"),
    one per store and one for the run. Sizes and times only: never the data."""
    ms = int((now if now is not None else time.time()) * 1000)
    base = {"level": "info", "service": "aws-backup", "version": version, "backup_id": backup_id, "direction": direction}
    lines = []
    for name, a in _done(summary).items():
        lines.append({**base, "msg": f"{direction} of {name} done", "store": name, "bytes": a["bytes"], "seconds": a["seconds"], "result": "ok"})
    run = {**base, "msg": message or f"{direction} {result}", "store": "all", "result": result}
    if result != "ok":
        run["level"] = "error"
    lines.append(run)
    ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ms / 1000))
    return [{"timestamp": ms, "message": json.dumps({"timestamp": ts, **line}, sort_keys=True)} for line in lines]


# ---------------------------------------------------------------- command line

def _load(path):
    with open(path) as f:
        return json.load(f)


def main(argv):
    cmd, args = argv[1], argv[2:]
    if cmd == "summary":
        with open(args[0]) as f:
            print(json.dumps(summary_from_output(f.read()), sort_keys=True))
    elif cmd == "manifest":
        print(json.dumps(build_manifest(_load(args[0]), args[1], args[2], int(args[3]), int(args[4])), indent=2, sort_keys=True))
    elif cmd == "artifacts":
        for a in sorted(_load(args[0])["artifacts"], key=lambda a: STORES.index(a["name"])):
            print(a["name"], a["file"], a["bytes"], a["sha256"], b64_of_hex(a["sha256"]))
    elif cmd == "check-schema":
        problems = schema_problems(_load(args[0]), code_migrations(args[1], args[2]))
        for p in problems:
            print(p, file=sys.stderr)
        return 1 if problems else 0
    elif cmd == "compare-counts":
        problems = count_problems(_load(args[0]), _load(args[1]))
        for p in problems:
            print(p, file=sys.stderr)
        return 1 if problems else 0
    elif cmd == "metrics":
        opt = [a if a else None for a in args[3:6]] + [None] * (6 - len(args))
        seconds, size, total = (float(opt[0]) if opt[0] else None), (int(opt[1]) if opt[1] else None), (float(opt[2]) if opt[2] else None)
        print(json.dumps(metric_data(args[0], args[1], _load(args[2]), seconds, size, total)))
    elif cmd == "log-events":
        print(json.dumps(log_events(args[0], args[1], args[2], args[3], _load(args[4]), args[5] if len(args) > 5 else "")))
    else:
        print(__doc__, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
