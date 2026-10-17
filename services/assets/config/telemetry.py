"""OpenTelemetry: traces, metrics and logs over OTLP/HTTP to the collector (docs/observability.md).

Configured by the standard OTEL_* environment variables. Without OTEL_EXPORTER_OTLP_ENDPOINT
nothing is set up, so tests and CI never export anything.

Gunicorn calls setup_telemetry in each worker after the fork (gunicorn.conf.py), and the Celery
worker calls setup_worker_telemetry in each pool process (config/celery.py): the batch
processors and the metric reader run background threads, and threads don't survive a fork.
"""

import os
from collections.abc import Callable

from opentelemetry import metrics, trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.celery import CeleryInstrumentor
from opentelemetry.instrumentation.django import DjangoInstrumentor
from opentelemetry.instrumentation.psycopg import PsycopgInstrumentor
from opentelemetry.instrumentation.system_metrics import SystemMetricsInstrumentor
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import OTELResourceDetector, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

from config import SERVICE_NAME

# The gateway and Docker probe /healthz every few seconds; those spans would drown real traffic.
EXCLUDED_URLS = "healthz"

PROCESS_METRICS: dict[str, list[str] | None] = {
    "process.cpu.time": ["user", "system"],
    "process.memory.usage": None,
    "process.thread.count": None,
    "cpython.gc.collections": None,
}


def enabled() -> bool:
    return bool(os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"))


def resource(version: str, instance_id: str) -> Resource:
    """service.* from the code and settings. OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES,
    when set, take precedence."""
    defaults = Resource.create(
        {
            "service.name": SERVICE_NAME,
            "service.version": version,
            "service.instance.id": instance_id,
        }
    )
    return defaults.merge(OTELResourceDetector().detect())


def setup_telemetry() -> Callable[[], None]:
    """For gunicorn workers: installs the global providers and instruments Django, psycopg and
    Celery (which puts the trace context into the tasks it sends). Must run before Django loads
    the WSGI app, because the instrumentation adds a middleware. Returns a function that flushes
    and stops the exporters; call it on shutdown."""
    if not enabled():
        return lambda: None
    shutdown = _install_providers()
    DjangoInstrumentor().instrument(excluded_urls=EXCLUDED_URLS)
    PsycopgInstrumentor().instrument()
    CeleryInstrumentor().instrument()  # type: ignore[no-untyped-call]
    return shutdown


def setup_worker_telemetry() -> Callable[[], None]:
    """For Celery pool processes: installs the global providers and instruments Celery (a span
    per task, continuing the trace of whoever sent it) and psycopg."""
    if not enabled():
        return lambda: None
    shutdown = _install_providers()
    PsycopgInstrumentor().instrument()
    CeleryInstrumentor().instrument()  # type: ignore[no-untyped-call]
    return shutdown


def _install_providers() -> Callable[[], None]:
    # The instrumentation reads settings.MIDDLEWARE; without this it would configure empty
    # settings. wsgi.py sets the same default.
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")
    from config.settings import env

    res = resource(env.service_version, env.service_instance_id)
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
