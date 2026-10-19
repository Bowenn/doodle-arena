"""OpenTelemetry: traces, metrics and logs over OTLP/HTTP to the collector (docs/observability.md).

Configured by the standard OTEL_* environment variables. Without OTEL_EXPORTER_OTLP_ENDPOINT
nothing is set up, so tests and CI never export anything.
"""

import os
from collections.abc import Callable
from typing import Any
from urllib.parse import parse_qsl, quote

from fastapi import FastAPI
from opentelemetry import metrics, trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
from opentelemetry.instrumentation.psycopg import PsycopgInstrumentor
from opentelemetry.instrumentation.sqlalchemy import SQLAlchemyInstrumentor
from opentelemetry.instrumentation.system_metrics import SystemMetricsInstrumentor
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import OTELResourceDetector, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace import Span
from sqlalchemy import Engine

from app import SERVICE_NAME
from app.context import AuthContext
from app.settings import Settings

# The gateway and Docker probe /healthz every few seconds; those spans would drown real traffic.
EXCLUDED_URLS = "healthz"

# What replaces every query value in a server span.
REDACTED = "REDACTED"

PROCESS_METRICS: dict[str, list[str] | None] = {
    "process.cpu.time": ["user", "system"],
    "process.memory.usage": None,
    "process.thread.count": None,
    "cpython.gc.collections": None,
}


def redact_query(query: str) -> str:
    """Keeps the parameter names and replaces every value. The Google callback's query holds the
    authorization code and state, which must never reach the trace backend; redacting every
    value means no list of sensitive names to keep up to date."""
    names = [name for name, _ in parse_qsl(query, keep_blank_values=True)]
    return "&".join(f"{quote(name)}={REDACTED}" for name in names)


def server_request_hook(span: Span, scope: dict[str, Any]) -> None:
    """Runs after the ASGI instrumentation has set the request's attributes, so these win."""
    raw = scope.get("query_string", b"")
    if not raw or not span.is_recording():
        return
    query = raw.decode("latin-1")
    redacted = redact_query(query)
    span.set_attribute("url.query", redacted)
    # The older semantic conventions put the query into these (OTEL_SEMCONV_STABILITY_OPT_IN).
    old = getattr(span, "attributes", None) or {}
    for key in ("http.target", "http.url"):
        value = old.get(key)
        if isinstance(value, str) and query in value:
            span.set_attribute(key, value.replace(query, redacted))


def enabled() -> bool:
    return bool(os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"))


def resource(settings: Settings) -> Resource:
    """service.* from the code and settings. OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES,
    when set, take precedence."""
    defaults = Resource.create(
        {
            "service.name": SERVICE_NAME,
            "service.version": settings.service_version,
            "service.instance.id": settings.service_instance_id,
        }
    )
    return defaults.merge(OTELResourceDetector().detect())


def setup_telemetry(app: FastAPI, settings: Settings) -> Callable[[], None]:
    """Installs the global tracer, meter and logger providers and instruments the app, its
    database engine and outgoing HTTP calls (to Google). Returns a function that flushes and
    stops the exporters; call it on shutdown."""
    if not enabled():
        return lambda: None
    shutdown = _install_providers(settings)
    ctx: AuthContext = app.state.ctx
    FastAPIInstrumentor.instrument_app(
        app, excluded_urls=EXCLUDED_URLS, server_request_hook=server_request_hook
    )
    # SQLAlchemy spans carry the statement and pool; psycopg spans the driver round trip.
    SQLAlchemyInstrumentor().instrument(engine=ctx.engine)
    PsycopgInstrumentor().instrument()
    # Patches httpx's transports, so the Google client created in create_app is covered too.
    HTTPXClientInstrumentor().instrument()
    return shutdown


def setup_process_telemetry(settings: Settings, engine: Engine) -> Callable[[], None]:
    """The same providers for a process without the web app (the outbox relay)."""
    if not enabled():
        return lambda: None
    shutdown = _install_providers(settings)
    SQLAlchemyInstrumentor().instrument(engine=engine)
    PsycopgInstrumentor().instrument()
    return shutdown


def _install_providers(settings: Settings) -> Callable[[], None]:
    res = resource(settings)
    # Exporters read the endpoint, headers and timeouts from OTEL_EXPORTER_OTLP_*; the metric
    # reader reads OTEL_METRIC_EXPORT_INTERVAL. Shutdown is explicit, not atexit.
    tracer_provider = TracerProvider(resource=res, shutdown_on_exit=False)
    tracer_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    meter_provider = MeterProvider(
        resource=res,
        metric_readers=[PeriodicExportingMetricReader(OTLPMetricExporter())],
        shutdown_on_exit=False,
    )
    logger_provider = LoggerProvider(resource=res, shutdown_on_exit=False)
    logger_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))
    trace.set_tracer_provider(tracer_provider)
    metrics.set_meter_provider(meter_provider)
    # Process runtime metrics (CPU, memory, threads, GC), like Go's runtime and the JVM metrics.
    # Host-wide system.* metrics are left out: they describe the machine, not the service.
    SystemMetricsInstrumentor(config=PROCESS_METRICS).instrument()
    set_logger_provider(logger_provider)

    def shutdown() -> None:
        tracer_provider.shutdown()
        meter_provider.shutdown()
        logger_provider.shutdown()

    return shutdown
