"""Reads from the Postgres read replica, with read-your-writes (ADR 0020).

The gallery and drawing pages read the replica, unless the caller has written something the
replica hasn't replayed yet. The marketplace answers each successful write with the primary's
WAL position in the cookie da_rw_lsn (path /api, so it reaches this service too): every service
shares one Postgres server, so one WAL orders all of them. A read with that cookie goes to the
replica only if the replica has replayed past it. Every read also needs the replica within
DB_REPLICA_MAX_LAG of the primary (StalenessFloor), so a replica that stopped replaying or lost the
primary never shows a moderator's decision late by more than that. The replica never makes a read
fail: one it can't answer is read from the primary.

The replica alias exists only when DB_REPLICA_HOST is set (config/settings.py).
"""

import logging
import re
import threading
import time
from collections import deque
from collections.abc import Callable

from django.conf import settings
from django.db import OperationalError, connections
from django.http import HttpRequest
from opentelemetry import metrics

REPLICA = "replica"
PRIMARY = "default"
COOKIE = "da_rw_lsn"
# A pg_lsn as Postgres prints it: two hexadecimal halves.
_LSN = re.compile(r"[0-9A-F]{1,8}/[0-9A-F]{1,8}")

log = logging.getLogger(__name__)
reads = metrics.get_meter(__name__).create_counter(
    "assets.db.reads",
    unit="{read}",
    description="Routed reads by target (primary or replica) and reason",
)


def _count(target: str, reason: str) -> None:
    reads.add(1, {"target": target, "reason": reason})


def lsn(position: str) -> int:
    """A pg_lsn as a number, so positions compare: the high half, then the low."""
    high, low = position.split("/")
    return int(high, 16) << 32 | int(low, 16)


def _position(alias: str, sql: str) -> str:
    with connections[alias].cursor() as cursor:
        cursor.execute(sql)
        row = cursor.fetchone()
    return str(row[0])


def replayed(alias: str) -> str:
    """The primary's WAL position the server behind alias has replayed up to. A server that isn't
    a standby has no replay position and holds everything it wrote."""
    return _position(alias, "SELECT COALESCE(pg_last_wal_replay_lsn(), pg_current_wal_lsn())::text")


class StalenessFloor:
    """How stale a replica read may be: the primary's WAL positions over the last max_lag seconds,
    sampled at most once a second as reads come in. A replica read needs the oldest of them
    replayed. Every sample in the window is at most max_lag old, so a replica past it is at most
    max_lag behind, whether it is slow, paused or cut off from the primary. With few reads the
    window holds only a recent sample, which is stricter, never looser."""

    SAMPLE_EVERY = 1.0

    def __init__(
        self,
        max_lag: float,
        primary_position: Callable[[], str],
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        # At least the sampling interval, so the newest sample always stays in the window.
        self._max_lag = max(max_lag, self.SAMPLE_EVERY)
        self._primary_position = primary_position
        self._clock = clock
        self._samples: deque[tuple[float, str]] = deque()
        self._lock = threading.Lock()

    def floor(self) -> str:
        """The position a replica must have replayed to answer a read now."""
        with self._lock:
            now = self._clock()
            if not self._samples or now - self._samples[-1][0] >= self.SAMPLE_EVERY:
                self._samples.append((now, self._primary_position()))
            while now - self._samples[0][0] > self._max_lag:
                self._samples.popleft()
            return self._samples[0][1]


_floor = StalenessFloor(
    settings.DB_REPLICA_MAX_LAG,
    lambda: _position(PRIMARY, "SELECT pg_current_wal_lsn()::text"),
)


def read[T](
    request: HttpRequest,
    query: Callable[[str], T],
    replica: str = REPLICA,
    floor: StalenessFloor | None = None,
) -> T:
    """Runs query(alias) on the replica when it has the caller's writes and is within max lag of
    the primary, else on the primary."""
    if replica not in settings.DATABASES:
        _count("primary", "no_replica")
        return query(PRIMARY)
    position = request.COOKIES.get(COOKIE, "")
    if not _LSN.fullmatch(position):
        position = ""
    # Outside the try: a primary that can't answer fails the read either way.
    required = (floor or _floor).floor()
    try:
        done = lsn(replayed(replica))
        if position and done < lsn(position):
            _count("primary", "behind")
            return query(PRIMARY)
        if done < lsn(required):
            _count("primary", "stale")
            return query(PRIMARY)
        _count("replica", "caught_up" if position else "no_write")
        return query(replica)
    except OperationalError as e:
        log.warning("replica read failed: reading the primary", extra={"error": str(e)})
        _count("primary", "replica_error")
        return query(PRIMARY)
