"""The admin sign-in bridge (ADR 0001): the token arrives once in an Authorization header with a
CSRF token, the role is required, and the session is scoped to /admin/ and lasts an hour."""

from datetime import timedelta
from typing import Any

import pytest
from django.contrib.auth.models import User
from django.test import Client
from django.utils import timezone

pytestmark = pytest.mark.django_db

SSO = "/admin/sso/"


@pytest.fixture
def browser() -> Client:
    # The test client skips CSRF checks unless asked; a browser doesn't.
    return Client(enforce_csrf_checks=True)


def csrf(browser: Client) -> str:
    resp = browser.get(SSO)
    assert resp.status_code == 200
    token: str = resp.json()["csrf_token"]
    return token


def sign_in(browser: Client, bearer: str | None, next_page: str = "") -> Any:
    headers = {"X-CSRFToken": csrf(browser)}
    if bearer is not None:
        headers["Authorization"] = f"Bearer {bearer}"
    url = SSO + (f"?next={next_page}" if next_page else "")
    return browser.post(url, headers=headers)


def test_get_issues_a_csrf_cookie_scoped_to_the_admin(browser: Client) -> None:
    resp = browser.get(SSO)

    assert resp.json()["csrf_token"]
    cookie = resp.cookies["csrftoken"]
    assert cookie["path"] == "/admin/"
    assert cookie["samesite"] == "Lax"
    assert "no-store" in resp["Cache-Control"]


def test_post_without_csrf_token_is_refused(browser: Client, signer: Any) -> None:
    browser.get(SSO)

    resp = browser.post(
        SSO, headers={"Authorization": f"Bearer {signer.token('u_mod', ['moderator'])}"}
    )

    assert resp.status_code == 403
    assert not User.objects.filter(username="u_mod").exists()


def test_missing_token_is_unauthenticated(browser: Client) -> None:
    resp = sign_in(browser, None)

    assert resp.status_code == 401
    assert resp.json()["error"]["code"] == "UNAUTHENTICATED"


def test_expired_token_is_unauthenticated(browser: Client, signer: Any) -> None:
    resp = sign_in(browser, signer.token("u_mod", ["moderator"], ttl=-120))

    assert resp.status_code == 401
    assert "sessionid" not in browser.cookies


def test_player_is_forbidden(browser: Client, signer: Any) -> None:
    resp = sign_in(browser, signer.token("u_ada", ["player"]))

    assert resp.status_code == 403
    assert resp.json()["error"]["code"] == "FORBIDDEN"
    assert "sessionid" not in browser.cookies
    assert not User.objects.get(username="u_ada").is_staff


def test_moderator_gets_a_one_hour_admin_session(browser: Client, signer: Any) -> None:
    before = timezone.now()
    resp = sign_in(browser, signer.token("u_mod", ["player", "moderator"], name="Mo"))

    assert resp.status_code == 200
    assert resp.json()["next"] == "/admin/drawings/reviewqueue/"
    cookie = resp.cookies["sessionid"]
    assert cookie["path"] == "/admin/"
    assert cookie["httponly"]
    assert cookie["samesite"] == "Lax"
    assert 3590 <= int(cookie["max-age"]) <= 3600

    user = User.objects.get(username="u_mod")
    assert user.is_staff and not user.is_superuser
    assert not user.has_usable_password()
    assert user.first_name == "Mo"
    assert user.has_perm("drawings.moderate_drawing")
    assert browser.get("/admin/drawings/reviewqueue/").status_code == 200

    # The expiry is absolute: later requests that change the session don't extend it.
    expiry = browser.session.get_expiry_date()
    assert before + timedelta(seconds=3590) <= expiry <= before + timedelta(seconds=3601)
    browser.get("/admin/drawings/reviewqueue/")
    assert browser.session.get_expiry_date() == expiry


def test_admin_role_is_superuser(browser: Client, signer: Any) -> None:
    assert sign_in(browser, signer.token("u_root", ["player", "admin"])).status_code == 200

    user = User.objects.get(username="u_root")
    assert user.is_staff and user.is_superuser


def test_revoked_role_loses_admin_access_at_next_sign_in(browser: Client, signer: Any) -> None:
    assert sign_in(browser, signer.token("u_mod", ["moderator"])).status_code == 200
    assert browser.get("/admin/").status_code == 200

    assert sign_in(browser, signer.token("u_mod", ["player"])).status_code == 403

    user = User.objects.get(username="u_mod")
    assert not user.is_staff
    assert not user.has_perm("drawings.moderate_drawing")
    # The earlier session in this browser ended too.
    assert browser.get("/admin/").status_code == 302


def test_next_is_limited_to_admin_pages(browser: Client, signer: Any) -> None:
    token = signer.token("u_mod", ["moderator"])
    assert sign_in(browser, token, "/admin/drawings/drawing/").json()["next"] == (
        "/admin/drawings/drawing/"
    )
    assert sign_in(browser, token, "//evil.example/").json()["next"] == (
        "/admin/drawings/reviewqueue/"
    )


def test_token_in_a_get_is_ignored(browser: Client, signer: Any) -> None:
    """GET only hands out a CSRF token; a token sent with it never signs anyone in."""
    token = signer.token("u_mod", ["moderator"])
    resp = browser.get(SSO, headers={"Authorization": f"Bearer {token}"})

    assert resp.status_code == 200
    assert "sessionid" not in browser.cookies
    assert not User.objects.filter(username="u_mod").exists()


def test_other_methods_are_not_allowed(browser: Client) -> None:
    assert browser.put(SSO, headers={"X-CSRFToken": csrf(browser)}).status_code == 405


def test_admin_login_page_sends_the_browser_to_the_web_handoff(browser: Client) -> None:
    resp = browser.get("/admin/")
    assert resp.status_code == 302
    assert resp["Location"] == "/admin/login/?next=/admin/"

    resp = browser.get("/admin/login/?next=/admin/drawings/drawing/")
    assert resp.status_code == 302
    assert resp["Location"] == "/moderate?next=%2Fadmin%2Fdrawings%2Fdrawing%2F"


def test_admin_login_page_refuses_open_redirects(browser: Client) -> None:
    resp = browser.get("/admin/login/?next=https://evil.example/")

    assert resp["Location"] == "/moderate?next=%2Fadmin%2Fdrawings%2Freviewqueue%2F"


def test_there_is_no_password_login(browser: Client) -> None:
    User.objects.create_user("u_pw", password="hunter2", is_staff=True)

    resp = browser.post("/admin/login/", {"username": "u_pw", "password": "hunter2"})

    assert resp.status_code in (403, 405)
    assert "sessionid" not in browser.cookies
