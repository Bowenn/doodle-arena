"""Entry point: `python -m app`. Uvicorn handles SIGTERM by finishing in-flight requests."""

import uvicorn

from app import SERVICE_NAME, telemetry
from app.logs import logging_config
from app.main import create_app
from app.settings import Settings


def main() -> None:
    settings = Settings()
    app = create_app(settings)
    # Before uvicorn.run, which applies log_config: the OTLP log handler needs the provider.
    shutdown_telemetry = telemetry.setup_telemetry(app, settings)
    try:
        uvicorn.run(
            app,
            host="0.0.0.0",
            port=settings.port,
            log_config=logging_config(
                SERVICE_NAME, settings.service_version, settings.log_level, telemetry.enabled()
            ),
            # Request telemetry comes from OpenTelemetry, not access logs.
            access_log=False,
            timeout_graceful_shutdown=10,
        )
    finally:
        shutdown_telemetry()  # flushes the last spans, metrics and logs


if __name__ == "__main__":
    main()
