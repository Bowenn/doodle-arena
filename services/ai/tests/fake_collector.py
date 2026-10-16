"""A stand-in for the OpenTelemetry Collector's OTLP/HTTP receiver, for tests. It records every
export request and decodes the protobuf payloads."""

import threading
from collections.abc import Iterable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import (
    ExportMetricsServiceRequest,
)
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from opentelemetry.proto.common.v1.common_pb2 import KeyValue
from opentelemetry.proto.logs.v1.logs_pb2 import LogRecord
from opentelemetry.proto.trace.v1.trace_pb2 import Span


def attributes(kvs: Iterable[KeyValue]) -> dict[str, str]:
    """String and int attributes as strings (enough for assertions)."""
    result = {}
    for kv in kvs:
        kind = kv.value.WhichOneof("value")
        result[kv.key] = str(getattr(kv.value, kind)) if kind else ""
    return result


class FakeCollector:
    def __init__(self) -> None:
        self._bodies: dict[str, list[bytes]] = {}
        self._lock = threading.Lock()
        collector = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                body = self.rfile.read(int(self.headers["Content-Length"]))
                with collector._lock:
                    collector._bodies.setdefault(self.path, []).append(body)
                self.send_response(200)
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, format: str, *args: Any) -> None:
                pass  # keep test output clean

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def __enter__(self) -> FakeCollector:
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._server.shutdown()
        self._server.server_close()

    @property
    def endpoint(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host!s}:{port}"

    def paths(self) -> set[str]:
        with self._lock:
            return set(self._bodies)

    def _requests(self, path: str) -> list[bytes]:
        with self._lock:
            return list(self._bodies.get(path, []))

    def resources(self) -> list[dict[str, str]]:
        """The resource attributes of every exported batch of spans."""
        return [
            attributes(rs.resource.attributes)
            for body in self._requests("/v1/traces")
            for rs in ExportTraceServiceRequest.FromString(body).resource_spans
        ]

    def spans(self) -> list[Span]:
        return [
            span
            for body in self._requests("/v1/traces")
            for rs in ExportTraceServiceRequest.FromString(body).resource_spans
            for ss in rs.scope_spans
            for span in ss.spans
        ]

    def metric_names(self) -> set[str]:
        return {
            m.name
            for body in self._requests("/v1/metrics")
            for rm in ExportMetricsServiceRequest.FromString(body).resource_metrics
            for sm in rm.scope_metrics
            for m in sm.metrics
        }

    def log_records(self) -> list[LogRecord]:
        return [
            record
            for body in self._requests("/v1/logs")
            for rl in ExportLogsServiceRequest.FromString(body).resource_logs
            for sl in rl.scope_logs
            for record in sl.log_records
        ]
