from datetime import timedelta

import httpx
from fastapi import FastAPI
from opentelemetry.metrics import Meter
from sqlalchemy import Engine

from app import SERVICE_NAME, api_key_routes
from app.context import AuthContext
from app.db import make_engine
from app.errors import install_error_handlers
from app.metrics import AuthMetrics
from app.oauth import GoogleClient
from app.routes import router
from app.settings import Settings
from app.tokens import SigningKey, TokenIssuer, TokenVerifier

# Every call to Google has a deadline.
GOOGLE_TIMEOUT = httpx.Timeout(5.0)


def build_context(
    settings: Settings,
    engine: Engine | None = None,
    http: httpx.Client | None = None,
    meter: Meter | None = None,
) -> AuthContext:
    # Fails fast without a key.
    key = SigningKey.load(settings.jwt_private_key_pem, settings.jwt_private_key_path)
    google = None
    if settings.google_configured:
        if len(settings.oauth_state_secret) < 32:
            raise ValueError("OAUTH_STATE_SECRET must be at least 32 characters")
        google = GoogleClient(
            settings.google_client_id,
            settings.google_client_secret,
            settings.google_redirect_uri,
            http or httpx.Client(timeout=GOOGLE_TIMEOUT),
        )
    ttl = timedelta(seconds=settings.access_token_ttl_seconds)
    return AuthContext(
        settings=settings,
        engine=engine or make_engine(settings.database_url()),
        issuer=TokenIssuer(key, settings.jwt_issuer, ttl),
        verifier=TokenVerifier({key.kid: key.public_key}, settings.jwt_issuer),
        google=google,
        metrics=AuthMetrics(meter),
    )


def create_app(settings: Settings | None = None, ctx: AuthContext | None = None) -> FastAPI:
    settings = settings or Settings()
    app = FastAPI(title=f"doodle-{SERVICE_NAME}", version=settings.service_version)
    app.state.ctx = ctx or build_context(settings)
    install_error_handlers(app)

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {
            "status": "ok",
            "service": SERVICE_NAME,
            "version": settings.service_version,
            "instance": settings.service_instance_id,
        }

    app.include_router(router)
    app.include_router(api_key_routes.router)
    app.include_router(api_key_routes.internal_router)
    return app
