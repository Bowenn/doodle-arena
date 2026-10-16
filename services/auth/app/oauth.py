"""Google sign-in: OAuth 2.0 authorization code flow with PKCE, state and an OIDC nonce.

The state, nonce and PKCE verifier travel in a short-lived, HMAC-signed, HttpOnly cookie
scoped to the callback path. The cookie binds the login to this browser (no login CSRF), and
nothing is stored server-side. Google enforces single use: a code can be exchanged only once.
"""

import base64
import hashlib
import hmac
import json
import secrets
import time
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlencode, urlsplit

import httpx
import jwt

GOOGLE_AUTHORIZE_URL = "https://accounts.google.com/o/oauth2/v2/auth"
GOOGLE_TOKEN_URL = "https://oauth2.googleapis.com/token"
GOOGLE_JWKS_URL = "https://www.googleapis.com/oauth2/v3/certs"
GOOGLE_ISSUERS = ["https://accounts.google.com", "accounts.google.com"]
# Only openid and profile: Google then never sends an email address (design doc 4.2).
SCOPES = "openid profile"
STATE_COOKIE_MAX_AGE = 600


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def _b64url_decode(text: str) -> bytes:
    return base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))


def new_code_verifier() -> str:
    """A PKCE code verifier: 64 random bytes, base64url (86 characters, within 43-128)."""
    return _b64url(secrets.token_bytes(64))


def code_challenge(verifier: str) -> str:
    """The S256 PKCE challenge (RFC 7636): base64url(SHA-256(verifier))."""
    return _b64url(hashlib.sha256(verifier.encode("ascii")).digest())


def safe_return_to(value: str | None) -> str | None:
    """Returns a local path to send the user to after login, or None if it isn't one.
    Rejects absolute and protocol-relative URLs, so login can't be an open redirect."""
    if not value:
        return "/"
    if not value.startswith("/") or value.startswith("//") or "\\" in value:
        return None
    parts = urlsplit(value)
    if parts.scheme or parts.netloc or any(ord(c) < 0x20 for c in value):
        return None
    return value


class StateError(Exception):
    """The OAuth state cookie is missing, tampered with, expired or doesn't match."""


@dataclass(frozen=True)
class LoginState:
    state: str
    nonce: str
    verifier: str
    return_to: str
    issued_at: int

    @classmethod
    def new(cls, return_to: str, now: float | None = None) -> LoginState:
        return cls(
            state=secrets.token_urlsafe(32),
            nonce=secrets.token_urlsafe(32),
            verifier=new_code_verifier(),
            return_to=return_to,
            issued_at=int(now if now is not None else time.time()),
        )

    def to_cookie(self, secret: str) -> str:
        payload = _b64url(json.dumps(self.__dict__, separators=(",", ":")).encode())
        signature = _b64url(hmac.new(secret.encode(), payload.encode(), hashlib.sha256).digest())
        return f"{payload}.{signature}"

    @classmethod
    def from_cookie(cls, value: str | None, secret: str, now: float | None = None) -> LoginState:
        if not value or value.count(".") != 1:
            raise StateError("missing or malformed state cookie")
        payload, signature = value.split(".")
        expected = hmac.new(secret.encode(), payload.encode(), hashlib.sha256).digest()
        if not hmac.compare_digest(_b64url(expected), signature):
            raise StateError("state cookie signature mismatch")
        try:
            state = cls(**json.loads(_b64url_decode(payload)))
        except (ValueError, TypeError) as e:
            raise StateError("state cookie payload invalid") from e
        age = (now if now is not None else time.time()) - state.issued_at
        if not 0 <= age <= STATE_COOKIE_MAX_AGE:
            raise StateError("state cookie expired")
        return state


class GoogleError(Exception):
    """Google could not be reached or returned an error (a 502 for our caller)."""


class IdTokenError(Exception):
    """Google's ID token failed verification."""


@dataclass(frozen=True)
class GoogleIdentity:
    subject: str
    name: str
    picture: str | None


class GoogleClient:
    def __init__(
        self, client_id: str, client_secret: str, redirect_uri: str, http: httpx.Client
    ) -> None:
        self.client_id = client_id
        self.client_secret = client_secret
        self.redirect_uri = redirect_uri
        self.http = http
        self._jwks: dict[str, Any] = {}
        # None until the first fetch. Not 0.0: time.monotonic() counts from boot, so shortly after
        # boot "now - 0.0" is under the refetch interval and the first fetch would never happen.
        self._jwks_fetched_at: float | None = None

    def authorize_url(self, login: LoginState) -> str:
        params = {
            "client_id": self.client_id,
            "redirect_uri": self.redirect_uri,
            "response_type": "code",
            "scope": SCOPES,
            "state": login.state,
            "nonce": login.nonce,
            "code_challenge": code_challenge(login.verifier),
            "code_challenge_method": "S256",
            "prompt": "select_account",
        }
        return f"{GOOGLE_AUTHORIZE_URL}?{urlencode(params)}"

    def exchange_code(self, code: str, verifier: str) -> str:
        """Exchanges the authorization code for Google's ID token."""
        try:
            resp = self.http.post(
                GOOGLE_TOKEN_URL,
                data={
                    "grant_type": "authorization_code",
                    "code": code,
                    "redirect_uri": self.redirect_uri,
                    "client_id": self.client_id,
                    "client_secret": self.client_secret,
                    "code_verifier": verifier,
                },
            )
        except httpx.HTTPError as e:
            raise GoogleError(f"token endpoint unreachable: {type(e).__name__}") from e
        if resp.status_code != 200:
            # Google's error body names the problem (for example invalid_grant) without secrets.
            reason = resp.json().get("error", "unknown") if resp.content else "empty body"
            raise GoogleError(f"token endpoint returned {resp.status_code}: {reason}")
        id_token = resp.json().get("id_token")
        if not isinstance(id_token, str):
            raise GoogleError("token response has no id_token")
        return id_token

    def verify_id_token(self, id_token: str, nonce: str) -> GoogleIdentity:
        try:
            kid = jwt.get_unverified_header(id_token).get("kid")
            key = self._signing_key(kid)
            claims = jwt.decode(
                id_token,
                key,
                algorithms=["RS256"],
                audience=self.client_id,
                issuer=GOOGLE_ISSUERS,
                leeway=30,
                options={"require": ["exp", "iat", "iss", "aud", "sub"]},
            )
        except jwt.PyJWTError as e:
            raise IdTokenError(str(e)) from e
        if not hmac.compare_digest(str(claims.get("nonce", "")), nonce):
            raise IdTokenError("nonce mismatch")
        return GoogleIdentity(
            subject=claims["sub"],
            name=str(claims.get("name") or "Player"),
            picture=claims.get("picture"),
        )

    def _signing_key(self, kid: object) -> Any:
        if not isinstance(kid, str):
            raise jwt.InvalidTokenError("ID token has no kid")
        # Google rotates keys; refetch on an unknown kid, at most once a minute.
        now = time.monotonic()
        stale = self._jwks_fetched_at is None or now - self._jwks_fetched_at > 60
        if kid not in self._jwks and stale:
            self._jwks = self._fetch_jwks()
            self._jwks_fetched_at = now
        if kid not in self._jwks:
            raise jwt.InvalidTokenError("ID token signed by an unknown Google key")
        return self._jwks[kid]

    def _fetch_jwks(self) -> dict[str, Any]:
        try:
            resp = self.http.get(GOOGLE_JWKS_URL)
            resp.raise_for_status()
        except httpx.HTTPError as e:
            raise GoogleError(f"JWKS unreachable: {type(e).__name__}") from e
        return {k["kid"]: jwt.PyJWK(k).key for k in resp.json().get("keys", []) if "kid" in k}
