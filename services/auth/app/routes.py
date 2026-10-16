import logging
from datetime import timedelta
from typing import Annotated, Any

from fastapi import APIRouter, Cookie, Depends, Query, Response
from fastapi.responses import RedirectResponse

from app import cookies, refresh, users
from app.context import AuthContext, get_context
from app.errors import ApiError
from app.oauth import (
    STATE_COOKIE_MAX_AGE,
    GoogleError,
    IdTokenError,
    LoginState,
    StateError,
    safe_return_to,
)
from app.tokens import jwks

log = logging.getLogger("auth")
router = APIRouter(prefix="/api/auth")
Ctx = Annotated[AuthContext, Depends(get_context)]


@router.get("/.well-known/jwks.json")
def jwks_document(ctx: Ctx, response: Response) -> dict[str, Any]:
    # Services cache this; five minutes keeps key rotation quick.
    response.headers["Cache-Control"] = "public, max-age=300"
    return jwks(ctx.verifier.public_keys)


@router.get("/google/login")
def google_login(ctx: Ctx, return_to: Annotated[str | None, Query()] = None) -> Response:
    if ctx.google is None:
        raise ApiError(503, "GOOGLE_NOT_CONFIGURED", "Google sign-in is not configured")
    target = safe_return_to(return_to)
    if target is None:
        raise ApiError(400, "INVALID_RETURN_TO", "return_to must be a path on this site")

    login = LoginState.new(target)
    response = RedirectResponse(ctx.google.authorize_url(login), status_code=302)
    response.headers["Cache-Control"] = "no-store"
    cookies.set_oauth_cookie(
        response,
        login.to_cookie(ctx.settings.oauth_state_secret),
        ctx.settings,
        max_age=STATE_COOKIE_MAX_AGE,
    )
    return response


@router.get("/google/callback")
def google_callback(
    ctx: Ctx,
    code: Annotated[str | None, Query()] = None,
    state: Annotated[str | None, Query()] = None,
    error: Annotated[str | None, Query()] = None,
    oauth_cookie: Annotated[str | None, Cookie(alias=cookies.OAUTH_COOKIE)] = None,
) -> Response:
    if ctx.google is None:
        raise ApiError(503, "GOOGLE_NOT_CONFIGURED", "Google sign-in is not configured")
    settings = ctx.settings
    try:
        login = LoginState.from_cookie(oauth_cookie, settings.oauth_state_secret)
    except StateError as e:
        log.warning("login failed", extra={"reason": "state_cookie", "detail": str(e)})
        raise ApiError(400, "OAUTH_STATE_INVALID", "login expired or was started elsewhere") from e

    if error:
        # The user cancelled at Google, or Google refused. Back to the app, signed out.
        log.info("login cancelled", extra={"reason": error[:50]})
        response = RedirectResponse(f"{settings.public_base_url}/?login_error=cancelled", 302)
        cookies.clear_oauth_cookie(response, settings)
        return response
    if not code or not state or state != login.state:
        log.warning("login failed", extra={"reason": "state_mismatch"})
        raise ApiError(400, "OAUTH_STATE_INVALID", "login expired or was started elsewhere")

    try:
        id_token = ctx.google.exchange_code(code, login.verifier)
        identity = ctx.google.verify_id_token(id_token, login.nonce)
    except GoogleError as e:
        log.error("login failed", extra={"reason": "google_unavailable", "detail": str(e)})
        raise ApiError(502, "GOOGLE_UNAVAILABLE", "could not reach Google; try again") from e
    except IdTokenError as e:
        log.warning("login failed", extra={"reason": "id_token_invalid", "detail": str(e)})
        raise ApiError(400, "ID_TOKEN_INVALID", "Google's answer could not be verified") from e

    ttl = timedelta(days=settings.refresh_token_ttl_days)
    with ctx.engine.begin() as conn:
        user = users.upsert_from_google(conn, identity)
        token = refresh.start_family(conn, user.id, ctx.now(), ttl)
    log.info("login succeeded", extra={"user_id": str(user.id)})

    response = RedirectResponse(f"{settings.public_base_url}{login.return_to}", status_code=302)
    response.headers["Cache-Control"] = "no-store"
    cookies.clear_oauth_cookie(response, settings)
    cookies.set_refresh_cookie(response, token, settings)
    return response
