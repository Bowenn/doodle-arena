"""RS256 access tokens and the JWKS that publishes their public keys (ADR 0001).

auth signs tokens with its private key. Every service, auth included, verifies them with the
public key: RS256 only, and issuer, audience, expiry and issue time are all checked.
"""

import base64
import hashlib
import json
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPrivateKey, RSAPublicKey
from jwt.algorithms import RSAAlgorithm

AUDIENCE = "doodle-api"
ALGORITHM = "RS256"
LEEWAY_SECONDS = 30


class InvalidTokenError(Exception):
    """The access token is missing, malformed, expired or not ours."""


def thumbprint(public_key: RSAPublicKey) -> str:
    """RFC 7638 JWK thumbprint, used as the key id (kid)."""
    jwk = RSAAlgorithm.to_jwk(public_key, as_dict=True)
    canonical = json.dumps({k: jwk[k] for k in ("e", "kty", "n")}, separators=(",", ":"))
    digest = hashlib.sha256(canonical.encode()).digest()
    return base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


@dataclass(frozen=True)
class SigningKey:
    kid: str
    private_key: RSAPrivateKey

    @property
    def public_key(self) -> RSAPublicKey:
        return self.private_key.public_key()

    @classmethod
    def from_pem(cls, pem: bytes) -> SigningKey:
        key = serialization.load_pem_private_key(pem, password=None)
        if not isinstance(key, RSAPrivateKey) or key.key_size < 2048:
            raise ValueError("JWT signing key must be an RSA private key of at least 2048 bits")
        return cls(kid=thumbprint(key.public_key()), private_key=key)

    @classmethod
    def from_file(cls, path: str) -> SigningKey:
        if not path:
            raise ValueError("JWT_PRIVATE_KEY_PATH is not set (run scripts/gen-jwt-key.sh)")
        return cls.from_pem(Path(path).read_bytes())


def jwks(public_keys: dict[str, RSAPublicKey]) -> dict[str, Any]:
    """The JSON Web Key Set for /.well-known/jwks.json. During a key rotation it holds the old
    and the new key, so tokens signed with either still verify."""
    keys = []
    for kid, key in public_keys.items():
        jwk = RSAAlgorithm.to_jwk(key, as_dict=True)
        keys.append(
            {"kty": "RSA", "use": "sig", "alg": ALGORITHM, "kid": kid, "n": jwk["n"], "e": jwk["e"]}
        )
    return {"keys": keys}


@dataclass(frozen=True)
class AccessClaims:
    user_id: str
    roles: list[str]


class TokenIssuer:
    def __init__(self, key: SigningKey, issuer: str, ttl: timedelta) -> None:
        self.key = key
        self.issuer = issuer
        self.ttl = ttl

    def issue(self, user_id: str, roles: list[str], now: datetime) -> tuple[str, int]:
        """Returns a signed access token and its lifetime in seconds."""
        claims = {
            "sub": user_id,
            "roles": roles,
            "iss": self.issuer,
            "aud": AUDIENCE,
            "iat": now,
            "exp": now + self.ttl,
            "jti": str(uuid.uuid4()),
        }
        token = jwt.encode(claims, self.key.private_key, ALGORITHM, headers={"kid": self.key.kid})
        return token, int(self.ttl.total_seconds())


class TokenVerifier:
    def __init__(self, public_keys: dict[str, RSAPublicKey], issuer: str) -> None:
        self.public_keys = public_keys
        self.issuer = issuer

    def verify(self, token: str) -> AccessClaims:
        try:
            kid = jwt.get_unverified_header(token).get("kid")
            key = self.public_keys.get(kid) if isinstance(kid, str) else None
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
        roles = claims.get("roles")
        if not isinstance(roles, list) or not all(isinstance(r, str) for r in roles):
            raise InvalidTokenError("roles claim must be a list of strings")
        return AccessClaims(user_id=claims["sub"], roles=roles)
