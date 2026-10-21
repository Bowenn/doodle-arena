"""Tests for scripts/aws/backups.py: python3 -m unittest discover -s scripts/aws (make test s=terraform)."""

import json
import os
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import backups  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))


def summary(names=None, result=None):
    names = backups.STORES if names is None else names
    s = {
        "artifacts": [{"name": n, "file": n + ".dump", "bytes": 100, "sha256": "ab" * 32, "seconds": 1.5} for n in names],
        "counts": {"pg-auth": {"public.users": 3}, "mongo-assets": {"replays": 2}},
        "migrations": {"auth": ["0003"], "marketplace": ["1", "2"], "notifier": ["3"], "assets": ["auth.0001_initial", "drawings.0004_moderation_claim"]},
    }
    if result:
        s["result"] = result
    return s


KNOWN = {
    "auth": {"0001", "0002", "0003"},
    "assets": {"drawings": {"0001_drawing", "0004_moderation_claim"}},
    "marketplace": {"1", "2", "3"},
    "notifier": {"1", "2", "3"},
}


class SummaryTest(unittest.TestCase):
    def test_reads_the_json_between_the_markers(self):
        out = "noise\nBACKUP-SUMMARY-BEGIN\n{\"artifacts\": []}\nBACKUP-SUMMARY-END\nmore"
        self.assertEqual(backups.summary_from_output(out), {"artifacts": []})

    def test_output_without_a_summary_is_an_error(self):
        with self.assertRaises(ValueError):
            backups.summary_from_output("pg_dump: error: connection refused")


class ManifestTest(unittest.TestCase):
    def test_lists_the_artifacts_in_restore_order(self):
        m = backups.build_manifest(summary(list(reversed(backups.STORES))), "20261021T180000Z-abc", "abc", 4, 4096, now=0)
        self.assertEqual([a["name"] for a in m["artifacts"]], backups.STORES)
        self.assertEqual(m["artifacts"][0]["name"], "mongo-assets")
        self.assertEqual(m["drawings"], {"prefix": "backups/20261021T180000Z-abc/drawings/", "objects": 4, "bytes": 4096})
        self.assertEqual(m["created"], "1970-01-01T00:00:00Z")

    def test_a_missing_store_is_refused(self):
        with self.assertRaises(ValueError):
            backups.build_manifest(summary(backups.STORES[:-1]), "id", "abc", 0, 0)

    def test_base64_of_the_hex_digest_is_what_s3_reports(self):
        # SHA-256 of the empty string.
        self.assertEqual(backups.b64_of_hex("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"),
                         "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=")


class SchemaTest(unittest.TestCase):
    def test_an_older_or_equal_schema_is_accepted(self):
        self.assertEqual(backups.schema_problems(summary(), KNOWN), [])

    def test_a_migration_the_code_lacks_is_refused_in_each_database(self):
        m = summary()
        m["migrations"] = {
            "auth": ["0004"],
            "marketplace": ["1", "7"],
            "notifier": ["4"],
            "assets": ["drawings.0005_new_field"],
        }
        problems = backups.schema_problems(m, KNOWN)
        self.assertEqual(len(problems), 4, problems)
        for db in ("auth", "marketplace", "notifier", "assets"):
            self.assertTrue(any(p.startswith(db) for p in problems), (db, problems))

    def test_a_dirty_notifier_migration_is_refused(self):
        m = summary()
        m["migrations"]["notifier"] = ["3 dirty"]
        self.assertIn("dirty", backups.schema_problems(m, KNOWN)[0])

    def test_djangos_own_apps_are_not_checked(self):
        m = summary()
        m["migrations"]["assets"] = ["admin.0099_from_a_newer_django"]
        self.assertEqual(backups.schema_problems(m, KNOWN), [])

    def test_an_empty_database_has_nothing_to_check(self):
        m = summary()
        m["migrations"] = {}
        self.assertEqual(backups.schema_problems(m, KNOWN), [])

    def test_reads_the_migrations_of_this_checkout(self):
        known = backups.code_migrations(ROOT, "HEAD")
        self.assertIn("0001", known["auth"])
        self.assertIn("1", known["marketplace"])
        self.assertIn("1", known["notifier"])
        self.assertIn("0001_drawing", known["assets"]["drawings"])
        self.assertNotIn("__init__", known["assets"].get("drawings", set()))


class CountsTest(unittest.TestCase):
    def test_equal_counts_pass(self):
        s = summary()
        self.assertEqual(backups.count_problems(s, {"counts": s["counts"]}), [])

    def test_a_missing_or_different_table_is_reported(self):
        restored = {"counts": {"pg-auth": {"public.users": 2}, "mongo-assets": {}}}
        problems = backups.count_problems(summary(), restored)
        self.assertEqual(len(problems), 2, problems)


class TelemetryTest(unittest.TestCase):
    def runs(self, data):
        return [{d["Name"]: d["Value"] for d in m["Dimensions"]} for m in data if m["MetricName"] == "backup.runs"]

    def test_a_good_run_reports_every_store(self):
        data = backups.metric_data("backup", "ok", summary(), 2.0, 4096, 30.0)
        runs = self.runs(data)
        self.assertEqual(len(runs), len(backups.STORES) + 2)  # the stores, drawings, all
        self.assertTrue(all(r["result"] == "ok" for r in runs))
        totals = [m for m in data if m["MetricName"] == "backup.duration" and m["Dimensions"][1]["Value"] == "all"]
        self.assertEqual(totals[0]["Value"], 30.0)

    def test_a_failed_run_blames_the_store_it_was_on(self):
        data = backups.metric_data("restore", "error", summary(backups.STORES[:2], "failed"))
        runs = self.runs(data)
        self.assertEqual([r["store"] for r in runs if r["result"] == "error"], [backups.STORES[2], "all"])
        self.assertEqual(len([r for r in runs if r["result"] == "ok"]), 2)

    def test_no_id_is_a_metric_dimension(self):
        for m in backups.metric_data("backup", "ok", summary(), 1.0, 1, 1.0):
            self.assertLessEqual({d["Name"] for d in m["Dimensions"]}, {"direction", "store", "result"})

    def test_log_lines_carry_the_backup_id_and_the_log_format(self):
        events = backups.log_events("backup", "error", "20261021T180000Z-abc", "abc", summary(["pg-auth"]), "upload failed", now=1)
        lines = [json.loads(e["message"]) for e in events]
        for line in lines:
            self.assertEqual(line["backup_id"], "20261021T180000Z-abc")
            for key in ("timestamp", "level", "service", "version", "msg"):
                self.assertIn(key, line)
        self.assertEqual(lines[-1]["level"], "error")
        self.assertEqual(lines[-1]["msg"], "upload failed")
        self.assertEqual(events[0]["timestamp"], 1000)


class CommandLineTest(unittest.TestCase):
    def test_artifacts_prints_one_line_per_store_in_restore_order(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(summary(list(reversed(backups.STORES))), f)
        try:
            out = subprocess.run([sys.executable, os.path.join(ROOT, "scripts/aws/backups.py"), "artifacts", f.name],
                                 check=True, capture_output=True, text=True).stdout.splitlines()
        finally:
            os.unlink(f.name)
        self.assertEqual([line.split()[0] for line in out], backups.STORES)
        self.assertEqual(len(out[0].split()), 5)


if __name__ == "__main__":
    unittest.main()
