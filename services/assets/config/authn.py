"""Access-token verification (ADR 0001): every service checks auth's RS256 JWTs itself, with the
same rules as auth and the lobby, and all of them pass the shared cases in fixtures/jwt/.

Checks: the signature against auth's JWKS, only RS256 (never "none", never HMAC), `iss`,
`aud == doodle-api`, `exp` and `iat` with 30 seconds of clock skew, and `sub` and `roles`.
"""

import json
import threading
import time
import urllib.request
from dataclasses import dataclass
from typing import Any, Protocol

import jwt
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPublicKey
from jwt.algorithms import RSAAlgorithm

ALGORITHM = "RS256"
AUDIENCE = "doodle-api"
LEEWAY_SECONDS = 30
JWKS_TIMEOUT_SECONDS = 3


class InvalidTokenError(Exception):
    """The token is missing, malformed, expired or not ours. The message is for logs only."""


@dataclass(frozen=True)
class Claims:
    user_id: str
    roles: list[str]
    # The display name; empty for tokens issued before auth added the claim.
    name: str


class KeySource(Protocol):
    def key(self, kid: str) -> RSAPublicKey | None: ...


class StaticKeys:
    """A fixed key set, for tests."""

    def __init__(self, keys: dict[str, RSAPublicKey]) -> None:
        self.keys = keys

    def key(self, kid: str) -> RSAPublicKey | None:
        return self.keys.get(kid)


def parse_jwks(doc: dict[str, Any]) -> dict[str, RSAPublicKey]:
    """The RS256 signing keys in a JWKS document, by kid. Other keys are skipped."""
    keys: dict[str, RSAPublicKey] = {}
    for jwk in doc.get("keys", []):
        if jwk.get("kty") != "RSA" or not jwk.get("kid"):
            continue
        if jwk.get("use", "sig") != "sig" or jwk.get("alg", ALGORITHM) != ALGORITHM:
            continue
        key = RSAAlgorithm.from_jwk(jwk)
        if isinstance(key, RSAPublicKey):
            keys[jwk["kid"]] = key
    return keys


class Jwks:
    """Fetches auth's key set and caches it. An unknown key id (auth rotated its key) triggers a
    refetch, at most once per min_refresh seconds, so forged key ids can't make this service
    hammer auth. A failed fetch is retried after a second: auth may still be starting."""

    def __init__(self, url: str, min_refresh: float = 30.0) -> None:
        self.url = url
        self.min_refresh = min_refresh
        self._keys: dict[str, RSAPublicKey] = {}
        self._lock = threading.Lock()
        self._next_fetch = 0.0

    def key(self, kid: str) -> RSAPublicKey | None:
        if (k := self._keys.get(kid)) is not None:
            return k
        with self._lock:
            # Another thread may have fetched the key while this one waited.
            if (k := self._keys.get(kid)) is not None:
                return k
            if time.monotonic() < self._next_fetch:
                return None
            try:
                keys = self._fetch()
            except Exception:
                self._next_fetch = time.monotonic() + 1.0
                raise
            self._next_fetch = time.monotonic() + self.min_refresh
            self._keys = keys
            return keys.get(kid)

    def _fetch(self) -> dict[str, RSAPublicKey]:
        with urllib.request.urlopen(self.url, timeout=JWKS_TIMEOUT_SECONDS) as resp:
            return parse_jwks(json.loads(resp.read(1 << 20)))


class TokenVerifier:
    def __init__(self, keys: KeySource, issuer: str) -> None:
        self.keys = keys
        self.issuer = issuer

    def verify(self, token: str) -> Claims:
        try:
            kid = jwt.get_unverified_header(token).get("kid")
            key = self.keys.key(kid) if isinstance(kid, str) else None
            if key is None:
                raise InvalidTokenError("unknown signing key")
            claims = jwt.decode(
                token,
                key,
                algorithms=[ALGORITHM],  # never "none", never HMAC with the public key
                audience=AUDIENCE,
                issuer=self.issuer,
                leeway=LEEWAY_SECONDS,
                options={"require": ["exp", "iat", "iss", "aud", "sub"]},
            )
        except jwt.PyJWTError as e:
            raise InvalidTokenError(str(e)) from e
        except OSError as e:  # the JWKS couldn't be fetched
            raise InvalidTokenError(f"fetch JWKS: {e}") from e
        roles = claims.get("roles")
        if not isinstance(roles, list) or not all(isinstance(r, str) for r in roles):
            raise InvalidTokenError("roles claim must be a list of strings")
        sub = claims["sub"]
        if not isinstance(sub, str) or not sub:
            raise InvalidTokenError("sub is required")
        name = claims.get("name")
        return Claims(user_id=sub, roles=roles, name=name if isinstance(name, str) else "")


def bearer_token(authorization: str) -> str | None:
    """The token in an `Authorization: Bearer <token>` header, or None."""
    scheme, _, token = authorization.partition(" ")
    if scheme.lower() != "bearer" or not token.strip():
        return None
    return token.strip()


_verifier: TokenVerifier | None = None
_verifier_lock = threading.Lock()


def verifier() -> TokenVerifier:
    """The process's verifier, built from settings on first use."""
    global _verifier
    with _verifier_lock:
        if _verifier is None:
            from django.conf import settings

            _verifier = TokenVerifier(Jwks(settings.AUTH_JWKS_URL), settings.JWT_ISSUER)
        return _verifier


def set_verifier_for_tests(v: TokenVerifier | None) -> None:
    global _verifier
    with _verifier_lock:
        _verifier = v
