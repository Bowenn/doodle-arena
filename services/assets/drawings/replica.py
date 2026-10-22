"""Reads from the Postgres read replica, with read-your-writes (ADR 0020).

The gallery and drawing pages read the replica, unless the caller has written something the
replica hasn't replayed yet. The marketplace answers each successful write with the primary's
WAL position in the cookie da_rw_lsn (path /api, so it reaches this service too): every service
shares one Postgres server, so one WAL orders all of them. A read with that cookie goes to the
replica only if the replica has replayed past it. The replica never makes a read fail: one it
can't answer is read from the primary.

The replica alias exists only when DB_REPLICA_HOST is set (config/settings.py).
"""

import logging
import re
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


def replayed_past(alias: str, position: str) -> bool:
    """Whether the server behind alias has replayed the primary's WAL up to position. A server
    that isn't a standby has no replay position and holds everything it wrote."""
    with connections[alias].cursor() as cursor:
        cursor.execute(
            "SELECT COALESCE(pg_last_wal_replay_lsn(), pg_current_wal_lsn()) >= %s::pg_lsn",
            [position],
        )
        row = cursor.fetchone()
    return bool(row and row[0])


def read[T](request: HttpRequest, query: Callable[[str], T], replica: str = REPLICA) -> T:
    """Runs query(alias) on the replica when it has the caller's writes, else on the primary."""
    if replica not in settings.DATABASES:
        _count("primary", "no_replica")
        return query(PRIMARY)
    position = request.COOKIES.get(COOKIE, "")
    if not _LSN.fullmatch(position):
        position = ""
    try:
        if position and not replayed_past(replica, position):
            _count("primary", "behind")
            return query(PRIMARY)
        _count("replica", "caught_up" if position else "no_write")
        return query(replica)
    except OperationalError as e:
        log.warning("replica read failed: reading the primary", extra={"error": str(e)})
        _count("primary", "replica_error")
        return query(PRIMARY)
