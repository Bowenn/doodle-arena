"""Telemetry setup: a no-op without a collector, and real OTLP exports with one. The export test
runs the service in a subprocess (the global providers can be set only once), against the
Testcontainers Postgres."""

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
from opentelemetry import trace
from opentelemetry.proto.trace.v1.trace_pb2 import Span
from opentelemetry.sdk.trace import TracerProvider
from sqlalchemy import Engine

from app import telemetry
from app.main import create_app
from app.settings import Settings
from tests.fake_collector import FakeCollector, attributes

SERVICE_ROOT = Path(__file__).resolve().parent.parent


def test_setup_without_an_endpoint_is_a_no_op(
    monkeypatch: pytest.MonkeyPatch, settings: Settings
) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    app = create_app(settings)

    shutdown = telemetry.setup_telemetry(app, settings)
    shutdown()

    assert not telemetry.enabled()
    assert not isinstance(trace.get_tracer_provider(), TracerProvider)
    assert not getattr(app, "_is_instrumented_by_opentelemetry", False)


# What `python -m app` does, with a test client instead of uvicorn. Settings come from the
# environment, as in the container.
SCRIPT = """
import logging
import logging.config

from fastapi.testclient import TestClient

from app import telemetry
from app.logs import logging_config
from app.main import create_app
from app.settings import Settings

settings = Settings()
app = create_app(settings)
shutdown = telemetry.setup_telemetry(app, settings)
logging.config.dictConfig(logging_config("auth", "1.2.3", "info", telemetry.enabled()))
client = TestClient(app, base_url="http://localhost:8080")
client.get("/healthz")
client.post(
    "/api/auth/refresh",
    headers={"Origin": "http://localhost:8080", "Cookie": "da_refresh=not-a-real-token"},
)
# Rejected (no state cookie), but the span must still not carry the code or the state.
client.get("/api/auth/google/callback?code=c0de-secret&state=st4te-secret")
shutdown()
"""


@pytest.mark.parametrize(
    ("query", "redacted"),
    [
        ("code=abc&state=xyz", "code=REDACTED&state=REDACTED"),
        ("return_to=%2Frooms%2Fr1", "return_to=REDACTED"),
        ("flag&a=1&a=2", "flag=REDACTED&a=REDACTED&a=REDACTED"),
        ("", ""),
    ],
)
def test_redact_query_keeps_names_only(query: str, redacted: str) -> None:
    assert telemetry.redact_query(query) == redacted


def test_exports_traces_metrics_and_logs_over_otlp(db: Engine, key_file: str) -> None:
    url = db.url
    env = {k: v for k, v in os.environ.items() if not k.startswith("OTEL_")} | {
        "OTEL_SEMCONV_STABILITY_OPT_IN": "http",
        "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=test",
        "SERVICE_VERSION": "1.2.3",
        "SERVICE_INSTANCE_ID": "auth-test",
        "JWT_PRIVATE_KEY_PATH": key_file,
        "DB_HOST": str(url.host),
        "DB_PORT": str(url.port),
        "DB_NAME": str(url.database),
        "DB_USER": str(url.username),
        "DB_PASSWORD": str(url.password),
    }
    with FakeCollector() as collector:
        run = subprocess.run(
            [sys.executable, "-c", SCRIPT],
            cwd=SERVICE_ROOT,
            env=env | {"OTEL_EXPORTER_OTLP_ENDPOINT": collector.endpoint},
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert run.returncode == 0, run.stderr

        resource = collector.resources()[0]
        assert resource["service.name"] == "auth"
        assert resource["service.version"] == "1.2.3"
        assert resource["service.instance.id"] == "auth-test"
        assert resource["deployment.environment"] == "test"

        spans = collector.spans()
        servers = {
            attributes(s.attributes)["url.path"]: s
            for s in spans
            if s.kind == Span.SpanKind.SPAN_KIND_SERVER
        }
        assert servers.keys() == {"/api/auth/refresh", "/api/auth/google/callback"}  # no /healthz
        server = servers["/api/auth/refresh"]

        # The OAuth code and state never leave the service, in any attribute of any span.
        callback = attributes(servers["/api/auth/google/callback"].attributes)
        assert callback["url.query"] == "code=REDACTED&state=REDACTED"
        for s in spans:
            for value in attributes(s.attributes).values():
                assert "c0de-secret" not in value and "st4te-secret" not in value, s.name
        # The refresh token lookup shows up in the request's trace (SQLAlchemy and psycopg).
        db_spans = [
            s
            for s in spans
            if s.kind == Span.SpanKind.SPAN_KIND_CLIENT and s.trace_id == server.trace_id
        ]
        assert len(db_spans) >= 2, [s.name for s in spans]

        assert {"auth.refreshes", "http.server.request.duration"} <= collector.metric_names()

        # The stdout line and the OTLP record of the rejection carry the request's trace.
        record = next(
            r for r in collector.log_records() if r.body.string_value == "refresh rejected"
        )
        assert record.trace_id == server.trace_id
        assert attributes(record.attributes)["reason"] == "invalid"
        line = next(json.loads(x) for x in run.stdout.splitlines() if "refresh rejected" in x)
        assert line["trace_id"] == server.trace_id.hex()
