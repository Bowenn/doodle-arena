"""The access-token verifier against the shared JWT fixtures (fixtures/jwt/, ADR 0001), so assets
accepts and rejects exactly the tokens auth and the lobby do; and the JWKS cache's refetch rule."""

import json
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

import pytest

from config.authn import (
    InvalidTokenError,
    Jwks,
    StaticKeys,
    TokenVerifier,
    bearer_token,
    parse_jwks,
)

FIXTURES = Path(__file__).resolve().parents[3] / "fixtures" / "jwt"
DOC: dict[str, Any] = json.loads((FIXTURES / "cases.json").read_text())
JWKS_DOC: dict[str, Any] = json.loads((FIXTURES / "jwks.json").read_text())


@pytest.fixture(scope="module")
def fixture_verifier() -> TokenVerifier:
    return TokenVerifier(StaticKeys(parse_jwks(JWKS_DOC)), DOC["issuer"])


@pytest.mark.parametrize("case", DOC["cases"], ids=lambda c: c["name"])
def test_shared_jwt_case(fixture_verifier: TokenVerifier, case: dict[str, Any]) -> None:
    if case["valid"]:
        claims = fixture_verifier.verify(case["token"])
        assert claims.user_id == case["sub"]
        assert claims.roles == case["roles"]
    else:
        with pytest.raises(InvalidTokenError):
            fixture_verifier.verify(case["token"])


@pytest.mark.parametrize(
    ("header", "token"),
    [
        ("Bearer abc.def.ghi", "abc.def.ghi"),
        ("bearer abc", "abc"),
        ("Bearer ", None),
        ("Basic abc", None),
        ("abc", None),
        ("", None),
    ],
)
def test_bearer_token(header: str, token: str | None) -> None:
    assert bearer_token(header) == token


class _JwksServer:
    """Serves the fixture JWKS and counts requests."""

    def __init__(self) -> None:
        self.hits = 0
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                outer.hits += 1
                body = json.dumps(JWKS_DOC).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args: Any) -> None:
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}/jwks.json"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()


@pytest.fixture
def jwks_server() -> Iterator[_JwksServer]:
    s = _JwksServer()
    yield s
    s.server.shutdown()


def test_jwks_is_fetched_once_and_cached(jwks_server: _JwksServer) -> None:
    verifier = TokenVerifier(Jwks(jwks_server.url), DOC["issuer"])
    valid = next(c for c in DOC["cases"] if c["name"] == "valid")

    for _ in range(3):
        assert verifier.verify(valid["token"]).user_id == valid["sub"]
    assert jwks_server.hits == 1


def test_unknown_key_ids_refetch_at_most_once_per_interval(jwks_server: _JwksServer) -> None:
    verifier = TokenVerifier(Jwks(jwks_server.url, min_refresh=60), DOC["issuer"])
    unknown = next(c for c in DOC["cases"] if c["name"] == "unknown_kid")

    for _ in range(5):
        with pytest.raises(InvalidTokenError):
            verifier.verify(unknown["token"])
    assert jwks_server.hits == 1


def test_an_unreachable_jwks_rejects_the_token() -> None:
    verifier = TokenVerifier(Jwks("http://127.0.0.1:9/jwks.json"), DOC["issuer"])
    valid = next(c for c in DOC["cases"] if c["name"] == "valid")

    with pytest.raises(InvalidTokenError):
        verifier.verify(valid["token"])
