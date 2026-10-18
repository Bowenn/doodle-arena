"""Telemetry setup: a no-op without a collector, and real OTLP exports with one. The export test
runs gunicorn with gunicorn.conf.py, so it covers the post_fork and worker_exit hooks."""

import os
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest
from django.conf import settings
from opentelemetry import trace
from opentelemetry.proto.trace.v1.trace_pb2 import Span
from opentelemetry.sdk.trace import TracerProvider

from config import telemetry
from tests.fake_collector import FakeCollector, attributes

SERVICE_ROOT = Path(__file__).resolve().parent.parent


def test_setup_without_an_endpoint_is_a_no_op(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)

    shutdown = telemetry.setup_telemetry()
    shutdown()

    assert not telemetry.enabled()
    assert not isinstance(trace.get_tracer_provider(), TracerProvider)
    assert not any("opentelemetry" in m for m in settings.MIDDLEWARE)
    assert settings.LOGGING["root"]["handlers"] == ["stdout"]


def test_each_process_gets_its_own_instance_id(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(
        "OTEL_RESOURCE_ATTRIBUTES",
        "service.instance.id=assets-worker-1,deployment.environment=local",
    )
    res = telemetry.resource("1.2.3", "hostname")

    assert (
        telemetry.process_resource(res, "p0").attributes["service.instance.id"]
        == "assets-worker-1-p0"
    )
    assert (
        telemetry.process_resource(res, "").attributes["service.instance.id"] == "assets-worker-1"
    )
    assert telemetry.process_resource(res, "p1").attributes["deployment.environment"] == "local"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port: int = s.getsockname()[1]
        return port


def get(url: str) -> int:
    try:
        with urllib.request.urlopen(url, timeout=2) as resp:
            status: int = resp.status
            return status
    except urllib.error.HTTPError as e:
        return e.code


def wait_until_up(url: str, proc: subprocess.Popen[str]) -> None:
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        assert proc.poll() is None, "gunicorn exited early"
        try:
            get(url)
            return
        except OSError:
            time.sleep(0.1)
    raise TimeoutError(url)


def test_gunicorn_workers_export_over_otlp() -> None:
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    with FakeCollector() as collector:
        env = {k: v for k, v in os.environ.items() if not k.startswith("OTEL_")} | {
            "OTEL_EXPORTER_OTLP_ENDPOINT": collector.endpoint,
            "OTEL_SEMCONV_STABILITY_OPT_IN": "http",
            "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=test",
            "DJANGO_SECRET_KEY": "test-only-not-a-secret",
            "SERVICE_VERSION": "1.2.3",
            "SERVICE_INSTANCE_ID": "assets-test",
            "WEB_CONCURRENCY": "1",
            # macOS only: the exporter's proxy lookup touches Objective-C code in the forked
            # worker, and the runtime kills it. Linux (the containers, CI) has no such check.
            "OBJC_DISABLE_INITIALIZE_FORK_SAFETY": "YES",
        }
        env.pop("DJANGO_SETTINGS_MODULE", None)  # the real settings, as in the container
        command = [sys.executable, "-m", "gunicorn", "config.wsgi:application"]
        proc = subprocess.Popen(
            [*command, "--config", "gunicorn.conf.py", "--bind", f"127.0.0.1:{port}"],
            cwd=SERVICE_ROOT,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        try:
            wait_until_up(f"{base}/healthz", proc)
            assert get(f"{base}/healthz") == 200
            assert get(f"{base}/no-such-page") == 404
        finally:
            proc.send_signal(signal.SIGTERM)  # worker_exit flushes the exporters
            output, _ = proc.communicate(timeout=30)
        assert proc.returncode == 0, output

        resource = collector.resources()[0]
        assert resource["service.name"] == "assets"
        assert resource["service.version"] == "1.2.3"
        assert resource["service.instance.id"] == "assets-test-w1"  # one id per gunicorn worker
        assert resource["deployment.environment"] == "test"

        spans = collector.spans()
        [server] = [s for s in spans if s.kind == Span.SpanKind.SPAN_KIND_SERVER]  # no /healthz
        assert attributes(server.attributes)["url.path"] == "/no-such-page"
        assert "http.server.request.duration" in collector.metric_names()

        # Django logs the 404 after the middleware chain, so outside the request's span.
        record = next(r for r in collector.log_records() if "Not Found" in r.body.string_value)
        assert attributes(record.attributes)["request"] == "<WSGIRequest: GET '/no-such-page'>"
        assert "Invalid type" not in output  # the request object was logged as a string
