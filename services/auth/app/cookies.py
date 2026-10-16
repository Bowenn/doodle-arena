"""The two cookies auth sets. Both are HttpOnly, so page scripts can never read them."""

from fastapi import Response

from app.settings import Settings

# Short-lived login state (state, nonce, PKCE verifier). SameSite=Lax because Google's redirect
# back to the callback is a cross-site top-level navigation, which Strict would drop.
OAUTH_COOKIE = "da_oauth"
OAUTH_COOKIE_PATH = "/api/auth/google/callback"

# The refresh token. Strict and scoped to /api/auth: only same-site calls to auth carry it.
REFRESH_COOKIE = "da_refresh"
REFRESH_COOKIE_PATH = "/api/auth"


def set_oauth_cookie(response: Response, value: str, settings: Settings, max_age: int) -> None:
    response.set_cookie(
        OAUTH_COOKIE,
        value,
        max_age=max_age,
        path=OAUTH_COOKIE_PATH,
        httponly=True,
        secure=settings.cookie_secure,
        samesite="lax",
    )


def clear_oauth_cookie(response: Response, settings: Settings) -> None:
    response.delete_cookie(
        OAUTH_COOKIE,
        path=OAUTH_COOKIE_PATH,
        httponly=True,
        secure=settings.cookie_secure,
        samesite="lax",
    )


def set_refresh_cookie(response: Response, token: str, settings: Settings) -> None:
    response.set_cookie(
        REFRESH_COOKIE,
        token,
        max_age=settings.refresh_token_ttl_days * 86400,
        path=REFRESH_COOKIE_PATH,
        httponly=True,
        secure=settings.cookie_secure,
        samesite="strict",
    )


def clear_refresh_cookie(response: Response, settings: Settings) -> None:
    response.delete_cookie(
        REFRESH_COOKIE,
        path=REFRESH_COOKIE_PATH,
        httponly=True,
        secure=settings.cookie_secure,
        samesite="strict",
    )
