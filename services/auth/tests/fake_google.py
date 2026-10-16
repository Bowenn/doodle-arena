"""A stand-in for Google's token endpoint and signing keys, for tests."""

import time
from typing import Any

import httpx
import jwt
import respx
from cryptography.hazmat.primitives.asymmetric import rsa
from jwt.algorithms import RSAAlgorithm

from app.oauth import GOOGLE_JWKS_URL, GOOGLE_TOKEN_URL

CLIENT_ID = "test-client.apps.googleusercontent.com"
CLIENT_SECRET = "test-client-secret"


class FakeGoogle:
    def __init__(self, router: respx.MockRouter) -> None:
        self.key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        self.kid = "google-test-key"
        self.router = router
        jwk = RSAAlgorithm.to_jwk(self.key.public_key(), as_dict=True)
        router.get(GOOGLE_JWKS_URL).mock(
            return_value=httpx.Response(200, json={"keys": [jwk | {"kid": self.kid}]})
        )
        self.token_route = router.post(GOOGLE_TOKEN_URL)

    def id_token(self, nonce: str, **overrides: Any) -> str:
        now = int(time.time())
        claims = {
            "iss": "https://accounts.google.com",
            "aud": CLIENT_ID,
            "sub": "google-sub-123",
            "name": "Ada Lovelace",
            "picture": "https://lh3.googleusercontent.com/a/ada",
            "nonce": nonce,
            "iat": now,
            "exp": now + 3600,
        } | overrides
        return jwt.encode(claims, self.key, "RS256", headers={"kid": self.kid})

    def answer_with(self, id_token: str) -> None:
        self.token_route.mock(
            return_value=httpx.Response(200, json={"id_token": id_token, "token_type": "Bearer"})
        )

    def fail_with(self, response: httpx.Response | Exception) -> None:
        if isinstance(response, Exception):
            self.token_route.mock(side_effect=response)
        else:
            self.token_route.mock(return_value=response)
