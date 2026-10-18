"""Access-token verification (ADR 0001): every service checks auth's RS256 JWTs itself, with the
same rules as auth and the lobby, and all of them pass the shared cases in fixtures/jwt/.

Checks: the signature against auth's JWKS, only RS256 (never "none", never HMAC), `iss`,
`aud == doodle-api`, `exp` and `iat` with 30 seconds of clock skew, and `sub` and `roles`.
"""

import json
import threading
import time
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, Protocol

import jwt
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPublicKey
from jwt.algorithms import RSAAlgorithm
from opentelemetry import metrics

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


# A fetched key set is refreshed after MAX_AGE_SECONDS, so a key that auth removes from its JWKS
# (rotated out, or withdrawn after a leak) stops being accepted within that time.
MAX_AGE_SECONDS = 600.0
# While refreshes fail, the last good set stays in use for at most MAX_STALE_SECONDS; then every
# token is refused until a fetch succeeds, so blocking the fetch can't keep a withdrawn key
# trusted. Only auth issues access tokens, and they last 15 minutes, so an auth outage this long
# leaves few valid tokens to refuse.
MAX_STALE_SECONDS = 3600.0

jwks_fetches = metrics.get_meter(__name__).create_counter(
    "authn.jwks.fetches",
    description="JWKS fetches by reason (max_age, unknown_kid) and result (success, failed)",
)


class Jwks:
    """Fetches auth's key set and caches it for MAX_AGE_SECONDS. An unknown key id (auth rotated
    its key) triggers a refetch too. Fetches happen at most once per min_refresh seconds, so
    forged key ids can't make this service hammer auth. A failed fetch is retried after a
    second: auth may still be starting."""

    def __init__(
        self, url: str, min_refresh: float = 30.0, clock: Callable[[], float] = time.monotonic
    ) -> None:
        self.url = url
        self.min_refresh = min_refresh
        self.clock = clock
        # The keys and when they were fetched, replaced together so a reader never pairs a new
        # set with an old time or the other way round. None until the first successful fetch.
        self._cache: tuple[dict[str, RSAPublicKey], float] | None = None
        self._lock = threading.Lock()  # one fetch at a time
        self._next_fetch = 0.0

    def key(self, kid: str) -> RSAPublicKey | None:
        k, age = self._cached(kid)
        if k is not None and age < MAX_AGE_SECONDS:
            return k
        usable = k is not None and age < MAX_STALE_SECONDS
        # Stale: one thread refreshes the set while the others keep using it, rather than all of
        # them waiting for a fetch that can take until its timeout when auth is unreachable.
        if not self._lock.acquire(blocking=not usable):
            return k
        try:
            # Another thread may have refreshed the set while this one waited, and the new set
            # may have dropped the key, so decide again from what is cached now.
            k, age = self._cached(kid)
            if k is not None and age < MAX_AGE_SECONDS:
                return k
            usable = k is not None and age < MAX_STALE_SECONDS
            try:
                keys = self._refresh("max_age" if k is not None else "unknown_kid")
            except Exception:
                if usable:
                    return k  # the last good set, until MAX_STALE_SECONDS
                raise
            if keys is not None:
                return keys.get(kid)
            if usable:
                return k
            if k is not None:
                raise InvalidTokenError(f"signing key {kid!r}: key set not refreshed for an hour")
            return None
        finally:
            self._lock.release()

    def _refresh(self, reason: str) -> dict[str, RSAPublicKey] | None:
        """Fetches the key set and caches it, or returns None when the last fetch was too recent
        to fetch again. The caller holds the lock."""
        now = self.clock()
        if now < self._next_fetch:
            return None
        try:
            keys = self._fetch()
        except Exception:
            jwks_fetches.add(1, {"reason": reason, "result": "failed"})
            self._next_fetch = now + 1.0
            raise
        jwks_fetches.add(1, {"reason": reason, "result": "success"})
        self._next_fetch = now + self.min_refresh
        self._cache = (keys, now)
        return keys

    def _cached(self, kid: str) -> tuple[RSAPublicKey | None, float]:
        """The cached key for kid (None if there is none) and the age of the cached set."""
        cache = self._cache
        if cache is None:
            return None, float("inf")
        keys, fetched_at = cache
        return keys.get(kid), self.clock() - fetched_at

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
