import logging
import re

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from opentelemetry import metrics

from app import SERVICE_NAME
from app.moderation import Abandoned, StubConfig, StubModerator, Unavailable, Verdict
from app.settings import Settings

log = logging.getLogger(__name__)
requests_counter = metrics.get_meter(__name__).create_counter(
    "ai.moderation.requests",
    description="Moderation calls by decision: approve, reject, review, unavailable, invalid, "
    "abandoned (the caller left first)",
)

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"
MAX_IMAGE_BYTES = 2 * 1024 * 1024
_DRAWING_ID = re.compile(r"^[0-9a-fA-F-]{1,64}$")


def _error(request: Request, status: int, code: str, message: str) -> JSONResponse:
    """The shared error format (design doc 6.3)."""
    body = {
        "error": {
            "code": code,
            "message": message,
            "request_id": request.headers.get("x-request-id", ""),
        }
    }
    return JSONResponse(body, status_code=status)


def create_app(settings: Settings | None = None, moderator: StubModerator | None = None) -> FastAPI:
    settings = settings or Settings()
    moderator = moderator or StubModerator(settings)
    app = FastAPI(title=f"doodle-{SERVICE_NAME}", version=settings.service_version)

    def internal_only(request: Request) -> None:
        # Both ports serve this app; operator endpoints answer only on the internal one.
        server = request.scope.get("server")
        if not server or server[1] != settings.internal_port:
            raise HTTPException(status_code=404)

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {
            "status": "ok",
            "service": SERVICE_NAME,
            "version": settings.service_version,
            "instance": settings.service_instance_id,
        }

    @app.post("/v1/moderate", response_model=Verdict)
    async def moderate(request: Request) -> Verdict | JSONResponse:
        """Body: the drawing as image/png. Header X-Drawing-Id: for logs only."""
        drawing_id = request.headers.get("x-drawing-id", "")
        extra = {"drawing_id": drawing_id} if _DRAWING_ID.match(drawing_id) else {}
        if request.headers.get("content-type", "").split(";")[0].strip() != "image/png":
            requests_counter.add(1, {"decision": "invalid"})
            return _error(request, 415, "UNSUPPORTED_MEDIA_TYPE", "send the drawing as image/png")
        image = await request.body()
        if len(image) > MAX_IMAGE_BYTES or not image.startswith(PNG_SIGNATURE):
            requests_counter.add(1, {"decision": "invalid"})
            return _error(request, 400, "INVALID_IMAGE", "the body is not a PNG under 2 MB")
        try:
            verdict = await moderator.decide(image, request.is_disconnected)
        except Abandoned as e:
            log.info("moderation abandoned: the caller left", extra={**extra, "reason": str(e)})
            requests_counter.add(1, {"decision": "abandoned"})
            return _error(request, 499, "CLIENT_CLOSED_REQUEST", "the caller left")
        except Unavailable as e:
            log.warning("moderation unavailable", extra={**extra, "reason": str(e)})
            requests_counter.add(1, {"decision": "unavailable"})
            return _error(request, 503, "MODERATION_UNAVAILABLE", "try again later")
        log.info("drawing moderated", extra={**extra, "decision": verdict.decision})
        requests_counter.add(1, {"decision": verdict.decision})
        return verdict

    @app.get("/internal/moderation-mode", response_model=StubConfig)
    def get_mode(request: Request) -> StubConfig:
        internal_only(request)
        return moderator.config

    @app.put("/internal/moderation-mode", response_model=StubConfig)
    def put_mode(config: StubConfig, request: Request) -> StubConfig:
        internal_only(request)
        moderator.configure(config)
        log.warning("moderation stub reconfigured", extra=config.model_dump())
        return config

    return app
