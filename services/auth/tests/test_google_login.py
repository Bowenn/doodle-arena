"""Google sign-in end to end against a real database, with Google's endpoints mocked."""

from urllib.parse import parse_qs, urlsplit

import httpx
import httpx2
import pytest
from fastapi.testclient import TestClient
from sqlalchemy import Engine, text

from app.oauth import code_challenge
from app.refresh import hash_token
from tests.fake_google import CLIENT_ID, CLIENT_SECRET, FakeGoogle


def start_login(client: TestClient, return_to: str = "/rooms/1") -> dict[str, str]:
    """Starts a login and returns the query parameters sent to Google."""
    resp = client.get(
        "/api/auth/google/login", params={"return_to": return_to}, follow_redirects=False
    )
    assert resp.status_code == 302
    location = urlsplit(resp.headers["location"])
    assert f"{location.scheme}://{location.netloc}{location.path}" == (
        "https://accounts.google.com/o/oauth2/v2/auth"
    )
    return {k: v[0] for k, v in parse_qs(location.query).items()}


def finish_login(client: TestClient, state: str, code: str = "auth-code") -> httpx2.Response:
    return client.get(
        "/api/auth/google/callback", params={"code": code, "state": state}, follow_redirects=False
    )


def test_login_redirects_to_google_with_pkce_state_and_nonce(client: TestClient) -> None:
    params = start_login(client)

    assert params["client_id"] == CLIENT_ID
    assert params["redirect_uri"] == "http://localhost:8080/api/auth/google/callback"
    assert params["response_type"] == "code"
    assert params["scope"] == "openid profile"  # never "email"
    assert params["code_challenge_method"] == "S256"
    assert len(params["state"]) >= 32 and len(params["nonce"]) >= 32
    cookie = next(c for c in client.cookies.jar if c.name == "da_oauth")
    assert cookie.path == "/api/auth/google/callback"


def test_login_sets_a_restricted_state_cookie(client: TestClient) -> None:
    resp = client.get("/api/auth/google/login", follow_redirects=False)
    set_cookie = resp.headers["set-cookie"].lower()
    for attribute in ["httponly", "samesite=lax", "path=/api/auth/google/callback", "max-age=600"]:
        assert attribute in set_cookie
    assert resp.headers["cache-control"] == "no-store"


def test_login_refuses_open_redirects(client: TestClient) -> None:
    resp = client.get("/api/auth/google/login", params={"return_to": "//evil.example"})
    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "INVALID_RETURN_TO"


def test_successful_login_creates_the_user_and_a_refresh_token(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    params = start_login(client, return_to="/rooms/1")
    google.answer_with(google.id_token(nonce=params["nonce"]))

    resp = finish_login(client, params["state"])

    assert resp.status_code == 302
    assert resp.headers["location"] == "http://localhost:8080/rooms/1"
    set_cookies = resp.headers.get_list("set-cookie")
    refresh_header = next(h for h in set_cookies if h.startswith("da_refresh="))
    for attribute in ["httponly", "samesite=strict", "path=/api/auth"]:
        assert attribute in refresh_header.lower()
    assert any(h.startswith('da_oauth=""') or "da_oauth=;" in h for h in set_cookies)

    # Google got the PKCE verifier that matches the challenge, and our client secret.
    sent = parse_qs(google.token_route.calls.last.request.content.decode())
    assert code_challenge(sent["code_verifier"][0]) == params["code_challenge"]
    assert sent["client_secret"] == [CLIENT_SECRET] and sent["code"] == ["auth-code"]

    with db.connect() as conn:
        user = conn.execute(text("SELECT * FROM users")).one()
        token_row = conn.execute(text("SELECT * FROM refresh_tokens")).one()
    assert (user.google_sub, user.display_name, user.roles) == (
        "google-sub-123",
        "Ada Lovelace",
        ["player"],
    )
    refresh_token = refresh_header.split(";")[0].split("=", 1)[1]
    assert bytes(token_row.token_hash) == hash_token(refresh_token)
    assert token_row.user_id == user.id


def test_second_login_updates_the_same_user(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    for name in ["Ada", "Ada Lovelace"]:
        params = start_login(client)
        google.answer_with(google.id_token(nonce=params["nonce"], name=name))
        assert finish_login(client, params["state"]).status_code == 302

    with db.connect() as conn:
        names = conn.execute(text("SELECT display_name FROM users")).scalars().all()
        families = conn.execute(text("SELECT count(DISTINCT family_id) FROM refresh_tokens"))
        assert (names, families.scalar()) == (["Ada Lovelace"], 2)


def test_callback_rejects_a_state_that_does_not_match_the_cookie(
    client: TestClient, google: FakeGoogle
) -> None:
    params = start_login(client)
    google.answer_with(google.id_token(nonce=params["nonce"]))

    resp = finish_login(client, state="attacker-state")

    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "OAUTH_STATE_INVALID"
    assert not google.token_route.called  # the code is never exchanged


def test_callback_without_the_state_cookie_is_login_csrf(
    client: TestClient, google: FakeGoogle
) -> None:
    # An attacker's own callback URL, opened in the victim's browser (no state cookie there).
    params = start_login(client)
    client.cookies.clear()

    resp = finish_login(client, params["state"])

    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "OAUTH_STATE_INVALID"


def test_callback_rejects_a_tampered_state_cookie(client: TestClient) -> None:
    params = start_login(client)
    value = client.cookies.get("da_oauth", path="/api/auth/google/callback")
    assert value
    client.cookies.set("da_oauth", value[:-3] + "AAA", path="/api/auth/google/callback")

    assert finish_login(client, params["state"]).status_code == 400


@pytest.mark.parametrize(
    "overrides",
    [
        {"aud": "someone-elses-client.apps.googleusercontent.com"},
        {"iss": "https://evil.example"},
        {"nonce": "replayed-nonce"},
        {"exp": 1_000_000_000},
    ],
)
def test_callback_rejects_bad_id_tokens(
    client: TestClient, google: FakeGoogle, db: Engine, overrides: dict[str, object]
) -> None:
    params = start_login(client)
    claims: dict[str, object] = {"nonce": params["nonce"]} | overrides
    google.answer_with(google.id_token(**claims))  # type: ignore[arg-type]

    resp = finish_login(client, params["state"])

    assert resp.status_code == 400
    assert resp.json()["error"]["code"] == "ID_TOKEN_INVALID"
    with db.connect() as conn:
        assert conn.execute(text("SELECT count(*) FROM users")).scalar() == 0


@pytest.mark.parametrize(
    "failure",
    [
        httpx.Response(400, json={"error": "invalid_grant"}),
        httpx.Response(503),
        httpx.ConnectTimeout("timed out"),
    ],
)
def test_callback_reports_google_failures_as_502(
    client: TestClient, google: FakeGoogle, failure: httpx.Response | Exception
) -> None:
    params = start_login(client)
    google.fail_with(failure)

    resp = finish_login(client, params["state"])

    assert resp.status_code == 502
    assert resp.json()["error"]["code"] == "GOOGLE_UNAVAILABLE"


def test_cancelled_login_returns_to_the_app_signed_out(client: TestClient) -> None:
    start_login(client)
    resp = client.get(
        "/api/auth/google/callback", params={"error": "access_denied"}, follow_redirects=False
    )
    assert resp.status_code == 302
    assert resp.headers["location"] == "http://localhost:8080/?login_error=cancelled"
    assert "da_refresh" not in resp.headers.get("set-cookie", "")


def test_login_without_google_credentials_returns_503(settings: object) -> None:
    from app.main import create_app

    client = TestClient(create_app(settings))  # type: ignore[arg-type]
    resp = client.get("/api/auth/google/login")
    assert resp.status_code == 503
    assert resp.json()["error"]["code"] == "GOOGLE_NOT_CONFIGURED"


def test_login_works_right_after_the_host_boots(
    client: TestClient, google: FakeGoogle, monkeypatch: pytest.MonkeyPatch
) -> None:
    # time.monotonic() counts from boot. Shortly after boot it is small, and the very first
    # fetch of Google's keys must still happen (regression: CI runners are freshly booted).
    monkeypatch.setattr("app.oauth.time.monotonic", lambda: 5.0)
    params = start_login(client)
    google.answer_with(google.id_token(nonce=params["nonce"]))

    assert finish_login(client, params["state"]).status_code == 302
