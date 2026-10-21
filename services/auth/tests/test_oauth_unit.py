import pytest

from app.oauth import LoginState, StateError, code_challenge, safe_return_to
from app.settings import Settings

SECRET = "s" * 40


def test_pkce_challenge_matches_rfc7636_appendix_b() -> None:
    verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
    assert code_challenge(verifier) == "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"


def test_new_login_state_uses_fresh_random_values() -> None:
    a, b = LoginState.new("/"), LoginState.new("/")
    assert len({a.state, b.state, a.nonce, b.nonce, a.verifier, b.verifier}) == 6
    assert 43 <= len(a.verifier) <= 128


def test_state_cookie_round_trip() -> None:
    login = LoginState.new("/rooms/1", now=1000)
    assert LoginState.from_cookie(login.to_cookie(SECRET), SECRET, now=1100) == login


@pytest.mark.parametrize(
    "tamper",
    [
        lambda c: c[:-2] + ("AA" if not c.endswith("AA") else "BB"),  # signature changed
        lambda c: "e30" + c[3:],  # payload changed
        lambda c: c.replace(".", ""),  # malformed
        lambda c: "",  # missing
    ],
)
def test_state_cookie_rejects_tampering(tamper: object) -> None:
    cookie = LoginState.new("/", now=1000).to_cookie(SECRET)
    with pytest.raises(StateError):
        LoginState.from_cookie(tamper(cookie), SECRET, now=1000)  # type: ignore[operator]


def test_state_cookie_rejects_another_secret_and_old_cookies() -> None:
    cookie = LoginState.new("/", now=1000).to_cookie(SECRET)
    with pytest.raises(StateError, match="signature"):
        LoginState.from_cookie(cookie, "x" * 40, now=1000)
    with pytest.raises(StateError, match="expired"):
        LoginState.from_cookie(cookie, SECRET, now=1000 + 601)


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        (None, "/"),
        ("", "/"),
        ("/", "/"),
        ("/rooms/42?invite=abc#chat", "/rooms/42?invite=abc#chat"),
        ("//evil.example/x", None),
        ("https://evil.example/", None),
        ("/\\evil.example", None),
        ("javascript:alert(1)", None),
        ("rooms/42", None),
        ("/ok\r\nSet-Cookie: x=y", None),
    ],
)
def test_return_to_only_allows_local_paths(value: str | None, expected: str | None) -> None:
    assert safe_return_to(value) == expected


@pytest.mark.parametrize(
    ("client_id", "client_secret", "configured"),
    [
        ("id", "secret", True),
        ("", "secret", False),
        ("id", "", False),
        ("unset", "unset", False),
        ("id", "unset", False),
    ],
)
def test_google_counts_as_set_up_only_with_real_values(
    client_id: str, client_secret: str, configured: bool
) -> None:
    # In AWS an SSM parameter can't be empty: one nobody has put yet holds "unset".
    settings = Settings(google_client_id=client_id, google_client_secret=client_secret)
    assert settings.google_configured is configured
