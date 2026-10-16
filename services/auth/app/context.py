"""Everything a request handler needs, built once at startup and stored on app.state."""

from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime

from fastapi import Request
from sqlalchemy import Engine

from app.oauth import GoogleClient
from app.settings import Settings
from app.tokens import TokenIssuer, TokenVerifier


def utc_now() -> datetime:
    return datetime.now(UTC)


@dataclass
class AuthContext:
    settings: Settings
    engine: Engine
    issuer: TokenIssuer
    verifier: TokenVerifier
    # None when no Google client is configured; sign-in then returns 503.
    google: GoogleClient | None
    now: Callable[[], datetime] = utc_now


def get_context(request: Request) -> AuthContext:
    ctx: AuthContext = request.app.state.ctx
    return ctx
