import logging
import uuid
from datetime import timedelta
from typing import Annotated, Any

from fastapi import APIRouter, Cookie, Depends, Header, Query, Request, Response
from fastapi.responses import JSONResponse, RedirectResponse

from app import cookies, refresh, users
from app.context import AuthContext, get_context
from app.errors import ApiError, error_response
from app.oauth import (
    STATE_COOKIE_MAX_AGE,
    GoogleError,
    IdTokenError,
    LoginState,
    StateError,
    safe_return_to,
)
from app.tokens import AccessClaims, InvalidTokenError, jwks

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
        ctx.metrics.login("failed")
        raise ApiError(400, "OAUTH_STATE_INVALID", "login expired or was started elsewhere") from e

    if error:
        # The user cancelled at Google, or Google refused. Back to the app, signed out.
        log.info("login cancelled", extra={"reason": error[:50]})
        response = RedirectResponse(f"{settings.public_base_url}/?login_error=cancelled", 302)
        cookies.clear_oauth_cookie(response, settings)
        return response
    if not code or not state or state != login.state:
        log.warning("login failed", extra={"reason": "state_mismatch"})
        ctx.metrics.login("failed")
        raise ApiError(400, "OAUTH_STATE_INVALID", "login expired or was started elsewhere")

    try:
        id_token = ctx.google.exchange_code(code, login.verifier)
        identity = ctx.google.verify_id_token(id_token, login.nonce)
    except GoogleError as e:
        log.error("login failed", extra={"reason": "google_unavailable", "detail": str(e)})
        ctx.metrics.login("failed")
        raise ApiError(502, "GOOGLE_UNAVAILABLE", "could not reach Google; try again") from e
    except IdTokenError as e:
        log.warning("login failed", extra={"reason": "id_token_invalid", "detail": str(e)})
        ctx.metrics.login("failed")
        raise ApiError(400, "ID_TOKEN_INVALID", "Google's answer could not be verified") from e

    ttl = timedelta(days=settings.refresh_token_ttl_days)
    with ctx.engine.begin() as conn:
        user = users.upsert_from_google(conn, identity)
        token = refresh.start_family(conn, user.id, ctx.now(), ttl)
    log.info("login succeeded", extra={"user_id": str(user.id)})
    ctx.metrics.login("success")

    response = RedirectResponse(f"{settings.public_base_url}{login.return_to}", status_code=302)
    response.headers["Cache-Control"] = "no-store"
    cookies.clear_oauth_cookie(response, settings)
    cookies.set_refresh_cookie(response, token, settings)
    return response


def check_origin(request: Request, ctx: AuthContext) -> None:
    """CSRF defense in depth for cookie-authenticated POSTs, on top of SameSite=Strict.
    Browsers always send Origin on cross-site POSTs, so a foreign Origin is refused; requests
    without one come from non-browser clients, which can't ride a user's cookie."""
    origin = request.headers.get("origin")
    if origin is not None and origin != ctx.settings.public_base_url:
        log.warning("refused cross-origin request", extra={"path": request.url.path})
        raise ApiError(403, "ORIGIN_NOT_ALLOWED", "request from another site")


def unauthorized_refresh(request: Request, ctx: AuthContext) -> Response:
    response = error_response(request, 401, "REFRESH_TOKEN_INVALID", "please sign in again")
    cookies.clear_refresh_cookie(response, ctx.settings)
    return response


@router.post("/refresh")
def refresh_access_token(
    request: Request,
    ctx: Ctx,
    refresh_cookie: Annotated[str | None, Cookie(alias=cookies.REFRESH_COOKIE)] = None,
) -> Response:
    check_origin(request, ctx)
    if not refresh_cookie:
        log.info("refresh rejected", extra={"reason": "missing_cookie"})
        ctx.metrics.refresh("invalid")
        return unauthorized_refresh(request, ctx)

    ttl = timedelta(days=ctx.settings.refresh_token_ttl_days)
    with ctx.engine.begin() as conn:
        result = refresh.rotate(conn, refresh_cookie, ctx.now(), ttl)
        user = None
        if result.outcome == "rotated" and result.user_id is not None:
            user = users.get(conn, result.user_id)

    if user is None or result.token is None:
        extra = {"reason": result.outcome, "user_id": str(result.user_id or "")}
        if result.outcome == "reuse_detected":
            extra["family_id"] = str(result.family_id)
            log.warning("refresh token reuse detected; family revoked", extra=extra)
            ctx.metrics.refresh("reuse_detected")
        else:
            if result.outcome == "rotated":  # the token was valid, but its user is gone
                extra["reason"] = "user_missing"
            log.info("refresh rejected", extra=extra)
            ctx.metrics.refresh("expired" if result.outcome == "expired" else "invalid")
        return unauthorized_refresh(request, ctx)

    access_token, expires_in = ctx.issuer.issue(str(user.id), user.roles, ctx.now())
    response = JSONResponse(
        {"access_token": access_token, "token_type": "Bearer", "expires_in": expires_in},
        headers={"Cache-Control": "no-store"},
    )
    cookies.set_refresh_cookie(response, result.token, ctx.settings)
    log.info("refresh rotated", extra={"user_id": str(user.id)})
    ctx.metrics.refresh("rotated")
    return response


@router.post("/logout", status_code=204)
def logout(
    request: Request,
    ctx: Ctx,
    refresh_cookie: Annotated[str | None, Cookie(alias=cookies.REFRESH_COOKIE)] = None,
) -> Response:
    check_origin(request, ctx)
    if refresh_cookie:
        with ctx.engine.begin() as conn:
            family_id = refresh.family_of(conn, refresh_cookie)
            if family_id is not None:
                refresh.revoke_family(conn, family_id, ctx.now(), "logout")
                log.info("logged out", extra={"family_id": str(family_id)})
    response = Response(status_code=204)
    cookies.clear_refresh_cookie(response, ctx.settings)
    return response


def current_claims(ctx: Ctx, authorization: Annotated[str | None, Header()] = None) -> AccessClaims:
    """The verified access token of the caller (Authorization: Bearer ...)."""
    scheme, _, token = (authorization or "").partition(" ")
    if scheme.lower() != "bearer" or not token:
        raise ApiError(401, "TOKEN_MISSING", "Authorization: Bearer token required")
    try:
        return ctx.verifier.verify(token.strip())
    except InvalidTokenError as e:
        raise ApiError(401, "TOKEN_INVALID", "access token is invalid or expired") from e


@router.get("/me")
def me(ctx: Ctx, claims: Annotated[AccessClaims, Depends(current_claims)]) -> dict[str, Any]:
    with ctx.engine.connect() as conn:
        user = users.get(conn, uuid.UUID(claims.user_id))
    if user is None:
        raise ApiError(401, "TOKEN_INVALID", "user no longer exists")
    return {
        "id": str(user.id),
        "display_name": user.display_name,
        "avatar_url": user.avatar_url,
        "roles": user.roles,
    }
