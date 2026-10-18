"""The moderation stub (design doc 4.8): POST /v1/moderate answers approve by default, and can be
told to reject, say "review" (unsure, so a human decides), slow down or fail, so the assets
service's timeouts, retries and review-queue fallback can be tested for real.

A real model replaces decide() in Stage 7; the HTTP contract stays.
"""

import asyncio
import random
import threading
import time
from collections.abc import Awaitable, Callable
from typing import Literal

from pydantic import BaseModel, Field

from app.settings import Mode, Settings

Decision = Literal["approve", "reject", "review"]
MODEL = "stub-1"


class StubConfig(BaseModel):
    mode: Mode
    # Added before every answer, including failures: a slow model, for testing timeouts.
    delay_ms: int = Field(ge=0, le=60_000)
    # Chance that a call fails with 503 even in a deciding mode: a flaky dependency.
    failure_rate: float = Field(ge=0.0, le=1.0)


class Verdict(BaseModel):
    decision: Decision
    reason: str
    model: str = MODEL


class Unavailable(Exception):
    """The stub was told to fail this call."""


class Abandoned(Exception):
    """The caller hung up (its timeout ran out) before the answer was ready."""


async def _never_disconnected() -> bool:
    return False


# How often a slow answer checks whether the caller is still there.
DISCONNECT_POLL_S = 0.05


class StubModerator:
    def __init__(self, settings: Settings, rng: random.Random | None = None) -> None:
        self._config = StubConfig(
            mode=settings.moderation_mode,
            delay_ms=settings.moderation_delay_ms,
            failure_rate=settings.moderation_failure_rate,
        )
        self._lock = threading.Lock()
        self._rng = rng or random.Random()

    @property
    def config(self) -> StubConfig:
        with self._lock:
            return self._config

    def configure(self, config: StubConfig) -> None:
        with self._lock:
            self._config = config

    async def decide(
        self, image: bytes, disconnected: Callable[[], Awaitable[bool]] = _never_disconnected
    ) -> Verdict:
        """While it waits out the delay it checks whether the caller is still connected, and stops
        when it isn't: a real model would otherwise keep a GPU busy on an answer nobody reads."""
        config = self.config
        deadline = time.monotonic() + config.delay_ms / 1000
        while (left := deadline - time.monotonic()) > 0:
            if await disconnected():
                raise Abandoned(f"caller left after {config.delay_ms / 1000 - left:.1f} s")
            await asyncio.sleep(min(left, DISCONNECT_POLL_S))
        if config.mode == "fail" or self._rng.random() < config.failure_rate:
            raise Unavailable(f"stub mode {config.mode}, failure rate {config.failure_rate}")
        decision: Decision = config.mode  # "fail" was handled above
        return Verdict(decision=decision, reason=f"stub: always {decision}")
