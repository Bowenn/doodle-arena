"""Refresh-token rotation with reuse detection (design doc 4.2), logout, and /me.
Runs against a real Postgres, because the guarantees depend on its row locks."""

import threading
import uuid
from concurrent.futures import ThreadPoolExecutor
from datetime import UTC, datetime, timedelta

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import Engine, text

from app import refresh
from tests.fake_google import FakeGoogle
from tests.test_google_login import finish_login, start_login

ORIGIN = {"Origin": "http://localhost:8080"}
PATH = "/api/auth"


def login(client: TestClient, google: FakeGoogle) -> str:
    """Signs in through the (mocked) Google flow and returns the refresh token."""
    params = start_login(client, return_to="/")
    google.answer_with(google.id_token(nonce=params["nonce"]))
    assert finish_login(client, params["state"]).status_code == 302
    token = client.cookies.get("da_refresh", path=PATH)
    assert token
    return token


def use(client: TestClient, token: str) -> int:
    """Presents a specific refresh token, the way a browser (or a thief) would."""
    domains = {c.domain for c in client.cookies.jar if c.name == "da_refresh"} or {""}
    client.cookies.delete("da_refresh")
    client.cookies.set("da_refresh", token, domain=domains.pop(), path=PATH)
    return client.post("/api/auth/refresh", headers=ORIGIN).status_code


def current_cookie(client: TestClient) -> str | None:
    return client.cookies.get("da_refresh", path=PATH)


def test_refresh_rotation(client: TestClient, google: FakeGoogle) -> None:
    first = login(client, google)

    resp = client.post("/api/auth/refresh", headers=ORIGIN)

    assert resp.status_code == 200
    body = resp.json()
    assert body["token_type"] == "Bearer" and body["expires_in"] == 900
    assert resp.headers["cache-control"] == "no-store"
    second = current_cookie(client)
    assert second and second != first  # rotated
    me = client.get("/api/auth/me", headers={"Authorization": f"Bearer {body['access_token']}"})
    assert me.status_code == 200 and me.json()["display_name"] == "Ada Lovelace"

    # The new token works and rotates again.
    assert use(client, second) == 200


def test_reusing_a_rotated_token_revokes_the_whole_family(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    stolen = login(client, google)
    assert use(client, stolen) == 200  # the real user refreshes: stolen is now used
    legit = current_cookie(client)
    assert legit

    assert use(client, stolen) == 401  # the thief replays it
    assert use(client, legit) == 401  # and the family is gone for everyone

    with db.connect() as conn:
        rows = conn.execute(text("SELECT revoked_reason FROM refresh_tokens")).scalars().all()
    assert rows and set(rows) == {"reuse_detected"}


def test_rejected_refresh_clears_the_cookie(client: TestClient, google: FakeGoogle) -> None:
    login(client, google)
    client.cookies.set("da_refresh", "not-a-real-token", path=PATH)

    resp = client.post("/api/auth/refresh", headers=ORIGIN)

    assert resp.status_code == 401
    assert resp.json()["error"]["code"] == "REFRESH_TOKEN_INVALID"
    assert (
        'da_refresh=""' in resp.headers["set-cookie"] or "Max-Age=0" in resp.headers["set-cookie"]
    )


def test_refresh_without_a_cookie_is_401(client: TestClient) -> None:
    assert client.post("/api/auth/refresh", headers=ORIGIN).status_code == 401


def test_expired_token_is_rejected_without_revoking_the_family(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    token = login(client, google)
    with db.begin() as conn:
        conn.execute(text("UPDATE refresh_tokens SET expires_at = now() - interval '1 second'"))

    assert use(client, token) == 401
    with db.connect() as conn:
        assert conn.execute(text("SELECT revoked_at FROM refresh_tokens")).scalar() is None


def test_logout_revokes_the_family(client: TestClient, google: FakeGoogle, db: Engine) -> None:
    token = login(client, google)

    resp = client.post("/api/auth/logout", headers=ORIGIN)

    assert resp.status_code == 204
    assert use(client, token) == 401
    with db.connect() as conn:
        reasons = conn.execute(text("SELECT revoked_reason FROM refresh_tokens")).scalars()
        assert set(reasons) == {"logout"}
    # Logging out again is harmless.
    assert client.post("/api/auth/logout", headers=ORIGIN).status_code == 204


@pytest.mark.parametrize("path", ["/api/auth/refresh", "/api/auth/logout"])
def test_cookie_endpoints_refuse_other_origins(
    client: TestClient, google: FakeGoogle, path: str
) -> None:
    login(client, google)
    resp = client.post(path, headers={"Origin": "https://evil.example"})
    assert resp.status_code == 403
    assert resp.json()["error"]["code"] == "ORIGIN_NOT_ALLOWED"


def test_concurrent_refreshes_of_one_token_let_exactly_one_win(db: Engine) -> None:
    """Ten parallel uses of one token: the row lock serializes them, one wins, and the other
    nine look like reuse, which revokes the family. That is why the web app refreshes
    single-flight."""
    user_id = uuid.uuid4()
    now = datetime.now(UTC)
    ttl = timedelta(days=30)
    with db.begin() as conn:
        conn.execute(
            text("INSERT INTO users (id, google_sub, display_name) VALUES (:id, 'g-c', 'C')"),
            {"id": user_id},
        )
        token = refresh.start_family(conn, user_id, now, ttl)

    barrier = threading.Barrier(10)

    def attempt() -> str:
        barrier.wait()
        with db.begin() as conn:
            return refresh.rotate(conn, token, datetime.now(UTC), ttl).outcome

    with ThreadPoolExecutor(max_workers=10) as pool:
        results = list(pool.map(lambda _: attempt(), range(10)))

    assert results.count("rotated") == 1
    assert results.count("reuse_detected") == 9
    with db.connect() as conn:
        live = conn.execute(text("SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL"))
        assert live.scalar() == 0


def test_me_requires_a_valid_bearer_token(client: TestClient, google: FakeGoogle) -> None:
    login(client, google)
    access = client.post("/api/auth/refresh", headers=ORIGIN).json()["access_token"]

    ok = client.get("/api/auth/me", headers={"Authorization": f"Bearer {access}"})
    assert ok.status_code == 200
    body = ok.json()
    assert set(body) == {"id", "display_name", "avatar_url", "roles"}  # no email
    assert body["roles"] == ["player"]

    for headers in [{}, {"Authorization": "Basic abc"}, {"Authorization": "Bearer junk"}]:
        resp = client.get("/api/auth/me", headers=headers)
        assert resp.status_code == 401, headers
        assert resp.json()["error"]["code"] in {"TOKEN_MISSING", "TOKEN_INVALID"}
