from datetime import timedelta
from typing import Annotated, Any

from fastapi import Depends, FastAPI, Response
from sqlalchemy import Engine

from app import SERVICE_NAME
from app.context import AuthContext, get_context
from app.db import make_engine
from app.errors import install_error_handlers
from app.settings import Settings
from app.tokens import SigningKey, TokenIssuer, TokenVerifier, jwks

Ctx = Annotated[AuthContext, Depends(get_context)]


def build_context(settings: Settings, engine: Engine | None = None) -> AuthContext:
    key = SigningKey.from_file(settings.jwt_private_key_path)  # fails fast without a key
    ttl = timedelta(seconds=settings.access_token_ttl_seconds)
    return AuthContext(
        settings=settings,
        engine=engine or make_engine(settings.database_url()),
        issuer=TokenIssuer(key, settings.jwt_issuer, ttl),
        verifier=TokenVerifier({key.kid: key.public_key}, settings.jwt_issuer),
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

    @app.get("/api/auth/.well-known/jwks.json")
    def jwks_document(ctx: Ctx, response: Response) -> dict[str, Any]:
        # Services cache this; five minutes keeps key rotation quick.
        response.headers["Cache-Control"] = "public, max-age=300"
        return jwks(ctx.verifier.public_keys)

    return app
