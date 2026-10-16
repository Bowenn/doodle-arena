"""auth's own verifier against the shared JWT fixtures (fixtures/jwt/, ADR 0001). The lobby
runs the same cases in Go, so every service accepts and rejects exactly the same tokens."""

import json
from pathlib import Path
from typing import Any

import pytest
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPublicKey
from jwt.algorithms import RSAAlgorithm

from app.tokens import InvalidTokenError, TokenVerifier

FIXTURES = Path(__file__).resolve().parents[3] / "fixtures" / "jwt"
DOC: dict[str, Any] = json.loads((FIXTURES / "cases.json").read_text())


@pytest.fixture(scope="module")
def verifier() -> TokenVerifier:
    keys: dict[str, RSAPublicKey] = {}
    for jwk in json.loads((FIXTURES / "jwks.json").read_text())["keys"]:
        key = RSAAlgorithm.from_jwk(jwk)
        assert isinstance(key, RSAPublicKey)
        keys[jwk["kid"]] = key
    return TokenVerifier(keys, DOC["issuer"])


@pytest.mark.parametrize("case", DOC["cases"], ids=lambda c: c["name"])
def test_shared_jwt_case(verifier: TokenVerifier, case: dict[str, Any]) -> None:
    if case["valid"]:
        claims = verifier.verify(case["token"])
        assert claims.user_id == case["sub"]
        assert claims.roles == case["roles"]
    else:
        with pytest.raises(InvalidTokenError):
            verifier.verify(case["token"])
