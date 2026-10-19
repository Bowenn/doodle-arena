"""Access-token rules from ADR 0001: RS256 only, and iss, aud, exp and iat are all checked."""

import base64
import hashlib
import hmac
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import jwt
import pytest
from fastapi.testclient import TestClient
from jwt.algorithms import RSAAlgorithm

from app.main import create_app
from app.settings import Settings
from app.tokens import (
    AUDIENCE,
    InvalidTokenError,
    SigningKey,
    TokenIssuer,
    TokenVerifier,
    jwks,
    thumbprint,
)
from tests.conftest import new_rsa_pem

ISSUER = "http://localhost:8080/api/auth"


@pytest.fixture(scope="module")
def key() -> SigningKey:
    return SigningKey.from_pem(new_rsa_pem())


@pytest.fixture
def issuer(key: SigningKey) -> TokenIssuer:
    return TokenIssuer(key, ISSUER, timedelta(minutes=15))


@pytest.fixture
def verifier(key: SigningKey) -> TokenVerifier:
    return TokenVerifier({key.kid: key.public_key}, ISSUER)


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def valid_claims(**overrides: Any) -> dict[str, Any]:
    now = int(datetime.now(UTC).timestamp())
    claims: dict[str, Any] = {"sub": "u-1", "roles": ["admin"], "iss": ISSUER, "aud": AUDIENCE}
    claims |= {"iat": now, "exp": now + 300}
    return claims | overrides


def unsigned_parts(alg: str, kid: str) -> str:
    """header.payload of a hand-made token with the given alg, for attack tests."""
    header = b64url(json.dumps({"alg": alg, "kid": kid, "typ": "JWT"}).encode())
    return header + "." + b64url(json.dumps(valid_claims()).encode())


def test_issued_token_verifies_and_carries_the_claims(
    issuer: TokenIssuer, verifier: TokenVerifier, key: SigningKey
) -> None:
    token, ttl = issuer.issue("u-1", ["player"], datetime.now(UTC))

    claims = verifier.verify(token)

    assert (claims.user_id, claims.roles, ttl) == ("u-1", ["player"], 900)
    header = jwt.get_unverified_header(token)
    assert header == {"alg": "RS256", "kid": key.kid, "typ": "JWT"}
    payload = jwt.decode(token, options={"verify_signature": False})
    assert payload["aud"] == AUDIENCE and payload["iss"] == ISSUER and payload["jti"]


def test_name_claim_carries_the_display_name(issuer: TokenIssuer) -> None:
    named, _ = issuer.issue("u-1", ["player"], datetime.now(UTC), name="Ada")
    unnamed, _ = issuer.issue("u-1", ["player"], datetime.now(UTC))

    assert jwt.decode(named, options={"verify_signature": False})["name"] == "Ada"
    assert "name" not in jwt.decode(unnamed, options={"verify_signature": False})


def test_rejects_expired_token(issuer: TokenIssuer, verifier: TokenVerifier) -> None:
    token, _ = issuer.issue("u-1", ["player"], datetime.now(UTC) - timedelta(hours=1))
    with pytest.raises(InvalidTokenError, match="expired"):
        verifier.verify(token)


def test_allows_small_clock_skew(issuer: TokenIssuer, verifier: TokenVerifier) -> None:
    # Expired 20 s ago: inside the 30 s leeway.
    token, _ = issuer.issue("u-1", ["player"], datetime.now(UTC) - timedelta(seconds=920))
    assert verifier.verify(token).user_id == "u-1"


@pytest.mark.parametrize(
    ("claim", "value"),
    [("aud", "another-api"), ("iss", "https://evil.example/api/auth")],
)
def test_rejects_wrong_audience_or_issuer(
    key: SigningKey, verifier: TokenVerifier, claim: str, value: str
) -> None:
    claims = valid_claims(**{claim: value})
    token = jwt.encode(claims, key.private_key, "RS256", headers={"kid": key.kid})
    with pytest.raises(InvalidTokenError):
        verifier.verify(token)


def test_rejects_token_signed_by_another_key(verifier: TokenVerifier) -> None:
    other = SigningKey.from_pem(new_rsa_pem())
    token, _ = TokenIssuer(other, ISSUER, timedelta(minutes=15)).issue(
        "u-1", ["admin"], datetime.now(UTC)
    )
    with pytest.raises(InvalidTokenError, match="unknown signing key"):
        verifier.verify(token)


def test_rejects_forged_token_reusing_our_kid(key: SigningKey, verifier: TokenVerifier) -> None:
    other = SigningKey.from_pem(new_rsa_pem())
    forged = SigningKey(kid=key.kid, private_key=other.private_key)
    token, _ = TokenIssuer(forged, ISSUER, timedelta(minutes=15)).issue(
        "u-1", ["admin"], datetime.now(UTC)
    )
    with pytest.raises(InvalidTokenError, match="Signature"):
        verifier.verify(token)


def test_rejects_alg_none(key: SigningKey, verifier: TokenVerifier) -> None:
    with pytest.raises(InvalidTokenError):
        verifier.verify(unsigned_parts("none", key.kid) + ".")


def test_rejects_hs256_signed_with_the_public_key(key: SigningKey, verifier: TokenVerifier) -> None:
    # Classic algorithm-confusion attack: HMAC "signed" with the public key, which is public.
    public_jwk = RSAAlgorithm.to_jwk(key.public_key).encode()
    signing_input = unsigned_parts("HS256", key.kid)
    sig = hmac.new(public_jwk, signing_input.encode(), hashlib.sha256).digest()
    with pytest.raises(InvalidTokenError):
        verifier.verify(f"{signing_input}.{b64url(sig)}")


def test_rejects_garbage(verifier: TokenVerifier) -> None:
    for token in ["", "not-a-jwt", "a.b.c"]:
        with pytest.raises(InvalidTokenError):
            verifier.verify(token)


def test_kid_is_the_rfc7638_thumbprint() -> None:
    # Test vector from RFC 7638, section 3.1.
    n = (
        "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJ"
        "ECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FD"
        "W2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4v"
        "MQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw"
    )
    jwk = {"kty": "RSA", "n": n, "e": "AQAB"}
    public_key = RSAAlgorithm.from_jwk(jwk)
    assert thumbprint(public_key) == "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"  # type: ignore[arg-type]


def test_jwks_document_lists_the_signing_key(settings: Settings, key_file: str) -> None:
    key = SigningKey.from_file(key_file)
    client = TestClient(create_app(settings))

    response = client.get("/api/auth/.well-known/jwks.json")

    assert response.status_code == 200
    assert response.headers["cache-control"] == "public, max-age=300"
    doc = response.json()
    assert doc == jwks({key.kid: key.public_key})
    (jwk,) = doc["keys"]
    assert {jwk["kty"], jwk["alg"], jwk["use"], jwk["kid"]} == {"RSA", "RS256", "sig", key.kid}
    assert "d" not in jwk  # never the private exponent


def test_signing_key_loads_from_pem_text_before_a_path(key_file: str) -> None:
    from_file = SigningKey.from_file(key_file)
    pem = Path(key_file).read_text()

    assert SigningKey.load(pem, "").kid == from_file.kid
    assert SigningKey.load(pem, "/no/such/file").kid == from_file.kid  # the text wins
    assert SigningKey.load("", key_file).kid == from_file.kid
    with pytest.raises(ValueError, match="JWT_PRIVATE_KEY_PATH"):
        SigningKey.load("", "")


def test_signing_key_must_be_rsa_2048_or_more() -> None:
    with pytest.raises(ValueError, match="JWT_PRIVATE_KEY_PATH"):
        SigningKey.from_file("")
