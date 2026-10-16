from fastapi import FastAPI

from app import SERVICE_NAME
from app.settings import Settings


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or Settings()
    app = FastAPI(title=f"doodle-{SERVICE_NAME}", version=settings.service_version)

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {
            "status": "ok",
            "service": SERVICE_NAME,
            "version": settings.service_version,
            "instance": settings.service_instance_id,
        }

    return app
