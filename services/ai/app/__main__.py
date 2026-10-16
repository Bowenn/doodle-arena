"""Entry point: `python -m app`. Uvicorn handles SIGTERM by finishing in-flight requests."""

import uvicorn

from app import SERVICE_NAME
from app.logs import logging_config
from app.main import create_app
from app.settings import Settings


def main() -> None:
    settings = Settings()
    uvicorn.run(
        create_app(settings),
        host="0.0.0.0",
        port=settings.port,
        log_config=logging_config(SERVICE_NAME, settings.service_version, settings.log_level),
        # Request telemetry comes from OpenTelemetry (task 1.5), not access logs.
        access_log=False,
        timeout_graceful_shutdown=10,
    )


if __name__ == "__main__":
    main()
