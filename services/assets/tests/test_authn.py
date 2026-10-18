"""The access-token verifier against the shared JWT fixtures (fixtures/jwt/, ADR 0001), so assets
accepts and rejects exactly the tokens auth and the lobby do; and the JWKS cache's refetch and
max-age rules."""

import itertools
import json
import threading
from collections.abc import Iterator
from concurrent.futures import ThreadPoolExecutor, wait
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

import pytest

from config.authn import (
    MAX_AGE_SECONDS,
    MAX_STALE_SECONDS,
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
    """Serves a JWKS document that a test can change (the fixture's by default), or fails with
    503, and counts requests. A test can also hold the next fetch open until it releases it."""

    def __init__(self) -> None:
        self.hits = 0
        self.doc: dict[str, Any] = JWKS_DOC
        self.failing = False
        self.hold: threading.Event | None = None  # set by a test to hold fetches open
        self.in_fetch = threading.Event()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                outer.hits += 1
                if outer.hold is not None:
                    outer.in_fetch.set()
                    outer.hold.wait(5)
                if outer.failing:
                    self.send_response(503)
                    self.end_headers()
                    return
                body = json.dumps(outer.doc).encode()
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
    if s.hold is not None:
        s.hold.set()
    s.server.shutdown()


class _Clock:
    """A fake monotonic clock for the JWKS cache. Tokens are still checked by the real clock."""

    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


VALID = next(c for c in DOC["cases"] if c["name"] == "valid")


def test_jwks_is_fetched_once_and_cached(jwks_server: _JwksServer) -> None:
    verifier = TokenVerifier(Jwks(jwks_server.url), DOC["issuer"])

    for _ in range(3):
        assert verifier.verify(VALID["token"]).user_id == VALID["sub"]
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

    with pytest.raises(InvalidTokenError):
        verifier.verify(VALID["token"])


def test_a_removed_key_is_refused_after_the_max_age(jwks_server: _JwksServer) -> None:
    clock = _Clock()
    verifier = TokenVerifier(Jwks(jwks_server.url, clock=clock), DOC["issuer"])
    assert verifier.verify(VALID["token"]).user_id == VALID["sub"]

    # auth withdraws the key. Until the max age the cached set still has it, and a hundred
    # requests spread over that time cause no fetch.
    jwks_server.doc = {"keys": []}
    for _ in range(100):
        clock.advance((MAX_AGE_SECONDS - 1) / 100)
        assert verifier.verify(VALID["token"]).user_id == VALID["sub"]
    assert jwks_server.hits == 1

    # Past the max age one request refreshes the set, and the key is gone.
    clock.advance(1)
    with pytest.raises(InvalidTokenError):
        verifier.verify(VALID["token"])
    assert jwks_server.hits == 2


def test_the_last_good_set_is_kept_until_the_max_staleness(jwks_server: _JwksServer) -> None:
    clock = _Clock()
    verifier = TokenVerifier(Jwks(jwks_server.url, clock=clock), DOC["issuer"])
    verifier.verify(VALID["token"])
    jwks_server.failing = True

    clock.advance(MAX_AGE_SECONDS)
    assert verifier.verify(VALID["token"]).user_id == VALID["sub"]
    assert jwks_server.hits == 2  # the refresh failed
    # The next attempt waits a second (then longer), and requests meanwhile use the cached set.
    verifier.verify(VALID["token"])
    assert jwks_server.hits == 2

    clock.advance(MAX_STALE_SECONDS - MAX_AGE_SECONDS - 1)
    verifier.verify(VALID["token"])
    assert jwks_server.hits == 3

    # Past the max staleness the set is refused, even before the next fetch is due (the second
    # failure backed off to 2 s).
    clock.advance(1)
    with pytest.raises(InvalidTokenError):
        verifier.verify(VALID["token"])
    assert jwks_server.hits == 3

    # auth is back: the next fetch, 2 s after the last failure, restores the set.
    jwks_server.failing = False
    clock.advance(1)
    assert verifier.verify(VALID["token"]).user_id == VALID["sub"]
    assert jwks_server.hits == 4


def test_fetches_back_off_while_auth_is_down(jwks_server: _JwksServer) -> None:
    """1 s, 2 s, 4 s, ... up to the minimum refresh interval: an outage costs auth at most one
    fetch per interval per process."""
    clock = _Clock()
    jwks_server.failing = True
    verifier = TokenVerifier(Jwks(jwks_server.url, clock=clock), DOC["issuer"])
    fetched_at: list[float] = []
    for _ in range(150):  # one request a second
        hits = jwks_server.hits
        with pytest.raises(InvalidTokenError):
            verifier.verify(VALID["token"])
        if jwks_server.hits != hits:
            fetched_at.append(clock.now)
        clock.advance(1)
    gaps = [b - a for a, b in itertools.pairwise(fetched_at)]
    assert gaps[:8] == [1, 2, 4, 8, 16, 30, 30, 30]


def test_a_stale_refresh_does_not_block_other_requests(jwks_server: _JwksServer) -> None:
    clock = _Clock()
    verifier = TokenVerifier(Jwks(jwks_server.url, clock=clock), DOC["issuer"])
    verifier.verify(VALID["token"])
    clock.advance(MAX_AGE_SECONDS)

    jwks_server.hold = threading.Event()
    errors: list[Exception] = []

    def refresh() -> None:
        try:
            verifier.verify(VALID["token"])
        except Exception as e:
            errors.append(e)

    refresher = threading.Thread(target=refresh)
    refresher.start()
    assert jwks_server.in_fetch.wait(5), "the stale set wasn't refreshed"

    # The refresh is in flight and holds the lock; other requests use the cached set at once.
    with ThreadPoolExecutor(max_workers=20) as pool:
        futures = [pool.submit(verifier.verify, VALID["token"]) for _ in range(20)]
        # Well under the fetch's 3-second timeout.
        done, _ = wait(futures, timeout=1)
        assert len(done) == 20, "requests waited for the refresh instead of using the cached set"
        for f in done:
            assert f.result().user_id == VALID["sub"]

    jwks_server.hold.set()
    refresher.join(5)
    assert not errors
    assert jwks_server.hits == 2
