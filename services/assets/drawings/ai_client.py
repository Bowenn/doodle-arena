"""Calls the AI service's moderation endpoint (design doc 4.8): POST /v1/moderate with the
drawing's thumbnail as image/png, answered with approve, reject or review.

Failures are sorted by whether waiting can fix them. Timeouts, connection errors, 429 and 5xx
raise ModerationUnavailable, which the task retries with backoff. Any other 4xx, or an answer
this client can't read, raises ModerationRefused: the drawing goes to the review queue at once.
"""

import threading
from dataclasses import dataclass
from typing import Literal

import httpx
from django.conf import settings

Decision = Literal["approve", "reject", "review"]
DECISIONS: tuple[Decision, ...] = ("approve", "reject", "review")


class ModerationUnavailable(Exception):
    """The AI service is slow, down or overloaded. Worth retrying."""


class ModerationRefused(Exception):
    """The AI service won't moderate this drawing (or answered nonsense). Retrying won't help."""


@dataclass(frozen=True)
class Verdict:
    decision: Decision
    reason: str
    model: str


_lock = threading.Lock()
_client: httpx.Client | None = None


def client() -> httpx.Client:
    """One per process, made after Celery's fork."""
    global _client
    with _lock:
        if _client is None:
            _client = httpx.Client(
                base_url=settings.AI_URL, timeout=httpx.Timeout(settings.AI_TIMEOUT_SECONDS)
            )
        return _client


def reset_client_for_tests() -> None:
    global _client
    with _lock:
        if _client is not None:
            _client.close()
        _client = None


def moderate(drawing_id: str, png: bytes) -> Verdict:
    try:
        resp = client().post(
            "/v1/moderate",
            content=png,
            headers={"Content-Type": "image/png", "X-Drawing-Id": drawing_id},
        )
    except httpx.TransportError as e:  # timeouts and connection errors
        raise ModerationUnavailable(f"{type(e).__name__}: {e}") from e
    if resp.status_code == 429 or resp.status_code >= 500:
        raise ModerationUnavailable(f"HTTP {resp.status_code}")
    if resp.status_code != 200:
        raise ModerationRefused(f"HTTP {resp.status_code}: {resp.text[:200]}")
    try:
        body = resp.json()
        decision = body["decision"]
        if decision not in DECISIONS:
            raise ValueError(f"unknown decision {decision!r}")
        return Verdict(
            decision=decision,
            reason=str(body.get("reason", ""))[:200],
            model=str(body.get("model", ""))[:64],
        )
    except (ValueError, KeyError, TypeError) as e:
        raise ModerationRefused(f"unreadable answer: {e}") from e
