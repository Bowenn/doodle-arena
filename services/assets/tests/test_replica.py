"""Read-your-writes with the read replica (ADR 0020, drawings/replica.py). The test database
stands in for the replica: a server that isn't a standby has replayed everything it wrote."""

from typing import Any

import pytest
from django.db import OperationalError, connection
from django.test import RequestFactory

from drawings import replica

pytestmark = pytest.mark.django_db


@pytest.fixture
def routed(monkeypatch: pytest.MonkeyPatch) -> list[tuple[str, str]]:
    seen: list[tuple[str, str]] = []
    monkeypatch.setattr(replica, "_count", lambda target, reason: seen.append((target, reason)))
    return seen


def request(position: str | None = None) -> Any:
    r = RequestFactory().get("/api/assets/drawings")
    if position is not None:
        r.COOKIES[replica.COOKIE] = position
    return r


def current_position() -> str:
    with connection.cursor() as c:
        c.execute("SELECT pg_current_wal_lsn()::text")
        return str(c.fetchone()[0])


def test_without_a_replica_every_read_is_the_primarys(routed: list[tuple[str, str]]) -> None:
    assert replica.read(request(), lambda db: db) == "default"
    assert routed == [("primary", "no_replica")]


def test_a_caller_without_writes_reads_the_replica(routed: list[tuple[str, str]]) -> None:
    replica.read(request(), lambda db: db, replica="default")
    assert routed == [("replica", "no_write")]


def test_a_write_the_replica_hasnt_replayed_reads_the_primary(
    routed: list[tuple[str, str]],
) -> None:
    replica.read(request("FFFFFFFF/FFFFFFFF"), lambda db: db, replica="default")
    assert routed == [("primary", "behind")]


def test_a_write_the_replica_has_replayed_reads_the_replica(routed: list[tuple[str, str]]) -> None:
    replica.read(request(current_position()), lambda db: db, replica="default")
    assert routed == [("replica", "caught_up")]


def test_a_cookie_that_isnt_a_position_is_ignored(routed: list[tuple[str, str]]) -> None:
    replica.read(request("0/0'; DROP TABLE drawings_drawing; --"), lambda db: db, replica="default")
    assert routed == [("replica", "no_write")]


def test_a_replica_that_is_down_never_fails_a_read(
    routed: list[tuple[str, str]], monkeypatch: pytest.MonkeyPatch
) -> None:
    def down(alias: str, position: str) -> bool:
        raise OperationalError("connection refused")

    monkeypatch.setattr(replica, "replayed_past", down)
    assert replica.read(request("0/1"), lambda db: db, replica="default") == "default"
    assert routed == [("primary", "replica_error")]
