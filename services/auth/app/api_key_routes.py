"""API key endpoints: the owner's own keys (JWT), and the internal check other services call."""

import logging
import uuid
from typing import Annotated, Any

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from pydantic import BaseModel, Field

from app import api_keys
from app.context import AuthContext, get_context
from app.errors import ApiError
from app.routes import current_claims
from app.tokens import AccessClaims

log = logging.getLogger("auth")
router = APIRouter(prefix="/api/auth/api-keys")
internal_router = APIRouter(prefix="/internal/api-keys")
Ctx = Annotated[AuthContext, Depends(get_context)]
Claims = Annotated[AccessClaims, Depends(current_claims)]


class CreateKey(BaseModel):
    name: str = Field(min_length=1, max_length=100)
    scopes: list[str] = Field(min_length=1)


class VerifyKey(BaseModel):
    key: str = Field(max_length=200)


def _view(key: api_keys.ApiKey) -> dict[str, Any]:
    return {
        "id": str(key.id),
        "name": key.name,
        "prefix": key.prefix,
        "scopes": key.scopes,
        "created_at": key.created_at.isoformat(),
        "last_used_at": key.last_used_at.isoformat() if key.last_used_at else None,
    }


@router.post("", status_code=201)
def create_key(ctx: Ctx, claims: Claims, body: CreateKey, response: Response) -> dict[str, Any]:
    unknown = set(body.scopes) - api_keys.SCOPES
    if unknown:
        raise ApiError(400, "INVALID_SCOPE", f"unknown scopes: {', '.join(sorted(unknown))}")
    try:
        with ctx.engine.begin() as conn:
            key, full = api_keys.create_within_cap(conn, claims.user_id, body.name, body.scopes)
    except api_keys.TooManyKeys:
        raise ApiError(
            409, "TOO_MANY_API_KEYS", f"at most {api_keys.MAX_ACTIVE_KEYS} active keys"
        ) from None
    # The key appears in this response only; it must not be cached anywhere.
    response.headers["Cache-Control"] = "no-store"
    log.info("api key created", extra={"user_id": claims.user_id, "key_prefix": key.prefix})
    return {**_view(key), "key": full}


@router.get("")
def list_keys(ctx: Ctx, claims: Claims) -> dict[str, Any]:
    with ctx.engine.connect() as conn:
        keys = api_keys.list_active(conn, claims.user_id)
    return {"api_keys": [_view(k) for k in keys]}


@router.delete("/{key_id}", status_code=204)
def revoke_key(ctx: Ctx, claims: Claims, key_id: str) -> Response:
    try:
        parsed = uuid.UUID(key_id)
    except ValueError:
        raise ApiError(404, "API_KEY_NOT_FOUND", "no such API key") from None
    with ctx.engine.begin() as conn:
        if not api_keys.revoke(conn, claims.user_id, parsed, ctx.now()):
            raise ApiError(404, "API_KEY_NOT_FOUND", "no such API key")
    log.info("api key revoked", extra={"user_id": claims.user_id, "key_id": key_id})
    return Response(status_code=204)


def internal_only(request: Request, ctx: Ctx) -> None:
    """One uvicorn serves both ports; this endpoint answers only on the internal one, which the
    gateway never routes and compose never publishes (ADR 0001)."""
    server = request.scope.get("server")
    if not server or server[1] != ctx.settings.internal_port:
        raise HTTPException(status_code=404)


@internal_router.post("/verify", dependencies=[Depends(internal_only)])
def verify_key(ctx: Ctx, body: VerifyKey) -> dict[str, Any]:
    with ctx.engine.begin() as conn:
        result, verified = api_keys.verify(conn, body.key, ctx.now())
    ctx.metrics.api_key_verification(result)
    if verified is None:
        # The prefix is public and identifies the key in logs; the secret never appears.
        parsed = api_keys.parse(body.key)
        log.info(
            "api key refused",
            extra={"reason": result, "key_prefix": parsed[0] if parsed else ""},
        )
        raise ApiError(401, "API_KEY_INVALID", "unknown, malformed or revoked API key")
    return {
        "key_id": str(verified.key_id),
        "user_id": verified.user_id,
        "scopes": verified.scopes,
    }
