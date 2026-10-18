"""Entry point: `python -m app`. One uvicorn server listens on two sockets: the service port
(8080) and the internal operator port (9091, see app.main). Uvicorn handles SIGTERM by finishing
in-flight requests."""

import socket

import uvicorn

from app import SERVICE_NAME, telemetry
from app.logs import logging_config
from app.main import create_app
from app.settings import Settings


def listen(port: int) -> socket.socket:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", port))
    return s


def main() -> None:
    settings = Settings()
    app = create_app(settings)
    # Before uvicorn starts, which applies log_config: the OTLP log handler needs the provider.
    shutdown_telemetry = telemetry.setup_telemetry(app, settings)
    config = uvicorn.Config(
        app,
        log_config=logging_config(
            SERVICE_NAME, settings.service_version, settings.log_level, telemetry.enabled()
        ),
        # Request telemetry comes from OpenTelemetry, not access logs.
        access_log=False,
        timeout_graceful_shutdown=10,
    )
    try:
        uvicorn.Server(config).run(sockets=[listen(settings.port), listen(settings.internal_port)])
    finally:
        shutdown_telemetry()  # flushes the last spans, metrics and logs


if __name__ == "__main__":
    main()
