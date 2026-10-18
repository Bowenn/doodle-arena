"""The moderation stub: its decisions, slow and failing modes, input checks, and the operator
endpoint that only answers on the internal port."""

import random
import time
from typing import Any

import pytest
from fastapi.testclient import TestClient

from app.main import create_app
from app.moderation import StubModerator
from app.settings import Settings

PNG = b"\x89PNG\r\n\x1a\n" + b"\x00" * 32
INTERNAL = "http://testserver:9091"


def client(**settings: Any) -> TestClient:
    s = Settings(**settings)
    return TestClient(create_app(s, StubModerator(s, random.Random(7))))


def moderate(
    c: TestClient, body: bytes = PNG, content_type: str = "image/png"
) -> Any:  # the TestClient's response
    return c.post(
        "/v1/moderate", content=body, headers={"Content-Type": content_type, "X-Drawing-Id": "01a1"}
    )


@pytest.mark.parametrize("mode", ["approve", "reject", "review"])
def test_deciding_modes(mode: str) -> None:
    resp = moderate(client(moderation_mode=mode))

    assert resp.status_code == 200
    assert resp.json() == {"decision": mode, "reason": f"stub: always {mode}", "model": "stub-1"}


def test_approves_by_default() -> None:
    assert moderate(client()).json()["decision"] == "approve"


def test_fail_mode_is_a_503_in_the_shared_error_format() -> None:
    resp = moderate(client(moderation_mode="fail"))

    assert resp.status_code == 503
    assert resp.json()["error"]["code"] == "MODERATION_UNAVAILABLE"


def test_failure_rate_fails_some_calls() -> None:
    c = client(moderation_failure_rate=0.5)

    codes = [moderate(c).status_code for _ in range(40)]

    assert 5 < codes.count(503) < 35
    assert codes.count(200) == 40 - codes.count(503)


def test_delay_slows_every_answer() -> None:
    c = client(moderation_delay_ms=300)

    start = time.monotonic()
    moderate(c)
    assert time.monotonic() - start >= 0.3


@pytest.mark.parametrize(
    ("body", "content_type", "status"),
    [
        (PNG, "image/jpeg", 415),
        (b"GIF89a....", "image/png", 400),
        (b"", "image/png", 400),
        (b"\x89PNG\r\n\x1a\n" + b"\x00" * (2 * 1024 * 1024), "image/png", 400),
    ],
)
def test_input_is_checked(body: bytes, content_type: str, status: int) -> None:
    assert moderate(client(), body, content_type).status_code == status


def test_the_mode_changes_at_runtime_on_the_internal_port() -> None:
    c = client()

    resp = c.put(
        f"{INTERNAL}/internal/moderation-mode",
        json={"mode": "reject", "delay_ms": 0, "failure_rate": 0},
    )
    assert resp.status_code == 200
    assert c.get(f"{INTERNAL}/internal/moderation-mode").json()["mode"] == "reject"
    assert moderate(c).json()["decision"] == "reject"


def test_operator_endpoints_are_hidden_on_the_service_port() -> None:
    c = client()

    assert c.get("/internal/moderation-mode").status_code == 404
    resp = c.put(
        "/internal/moderation-mode", json={"mode": "fail", "delay_ms": 0, "failure_rate": 0}
    )
    assert resp.status_code == 404
    assert moderate(c).json()["decision"] == "approve"


def test_a_bad_mode_is_refused() -> None:
    c = client()

    resp = c.put(
        f"{INTERNAL}/internal/moderation-mode",
        json={"mode": "chaos", "delay_ms": 0, "failure_rate": 2},
    )
    assert resp.status_code == 422


def test_a_slow_answer_stops_when_the_caller_leaves() -> None:
    import asyncio

    from app.moderation import Abandoned

    moderator = StubModerator(Settings(moderation_delay_ms=5000))
    checks = 0

    async def disconnected() -> bool:
        nonlocal checks
        checks += 1
        return checks > 3  # the caller hangs up after about 150 ms

    start = time.monotonic()
    with pytest.raises(Abandoned):
        asyncio.run(moderator.decide(PNG, disconnected))
    assert time.monotonic() - start < 1.0  # not the full 5 s


def test_a_slow_answer_still_arrives_for_a_patient_caller() -> None:
    import asyncio

    moderator = StubModerator(Settings(moderation_delay_ms=200))

    verdict = asyncio.run(moderator.decide(PNG))

    assert verdict.decision == "approve"
