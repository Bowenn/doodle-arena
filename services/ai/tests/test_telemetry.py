"""Telemetry setup: a no-op without a collector, and real OTLP exports with one. The export test
runs the service in a subprocess, because the global providers can be set only once."""

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient
from opentelemetry import trace
from opentelemetry.proto.trace.v1.trace_pb2 import Span
from opentelemetry.sdk.trace import TracerProvider

from app import telemetry
from app.main import create_app
from app.settings import Settings
from tests.fake_collector import FakeCollector, attributes

SERVICE_ROOT = Path(__file__).resolve().parent.parent


def test_setup_without_an_endpoint_is_a_no_op(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    app = create_app(Settings())

    shutdown = telemetry.setup_telemetry(app, Settings())
    shutdown()

    assert not telemetry.enabled()
    assert not isinstance(trace.get_tracer_provider(), TracerProvider)
    assert not getattr(app, "_is_instrumented_by_opentelemetry", False)
    assert TestClient(app).get("/healthz").status_code == 200


# What `python -m app` does, with a test client instead of uvicorn.
SCRIPT = """
import logging
import logging.config

from fastapi.testclient import TestClient
from opentelemetry import trace

from app import telemetry
from app.logs import logging_config
from app.main import create_app
from app.settings import Settings

settings = Settings(service_version="1.2.3", service_instance_id="ai-test")
app = create_app(settings)
shutdown = telemetry.setup_telemetry(app, settings)
logging.config.dictConfig(logging_config("ai", "1.2.3", "info", telemetry.enabled()))
client = TestClient(app)
client.get("/healthz")
client.get("/no-such-page")
with trace.get_tracer("test").start_as_current_span("ai.test"):
    logging.getLogger("ai").info("inside a span")
shutdown()
"""


def otel_env(endpoint: str) -> dict[str, str]:
    env = {k: v for k, v in os.environ.items() if not k.startswith("OTEL_")}
    return env | {
        "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
        "OTEL_SEMCONV_STABILITY_OPT_IN": "http",
        "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=test,service.version=from-env",
    }


def server_paths(spans: list[Span]) -> list[str]:
    return [
        attributes(s.attributes).get("url.path", "")
        for s in spans
        if s.kind == Span.SpanKind.SPAN_KIND_SERVER
    ]


def test_exports_traces_metrics_and_logs_over_otlp() -> None:
    with FakeCollector() as collector:
        run = subprocess.run(
            [sys.executable, "-c", SCRIPT],
            cwd=SERVICE_ROOT,
            env=otel_env(collector.endpoint),
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert run.returncode == 0, run.stderr

        assert {"/v1/traces", "/v1/metrics", "/v1/logs"} <= collector.paths()
        resource = collector.resources()[0]
        assert resource["service.name"] == "ai"
        assert resource["service.instance.id"] == "ai-test"
        assert resource["deployment.environment"] == "test"
        assert resource["service.version"] == "from-env"  # the environment wins

        spans = collector.spans()
        assert server_paths(spans) == ["/no-such-page"]  # /healthz is not traced
        assert "http.server.request.duration" in collector.metric_names()

        # The stdout line and the OTLP record of one log carry the trace of the span around it.
        test_span = next(s for s in spans if s.name == "ai.test")
        record = next(r for r in collector.log_records() if r.body.string_value == "inside a span")
        line = next(json.loads(x) for x in run.stdout.splitlines() if "inside a span" in x)
        assert record.trace_id == test_span.trace_id
        assert line["trace_id"] == test_span.trace_id.hex()
        assert line["span_id"] == test_span.span_id.hex()
