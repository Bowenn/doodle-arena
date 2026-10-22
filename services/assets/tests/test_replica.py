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
    def down(alias: str) -> str:
        raise OperationalError("connection refused")

    monkeypatch.setattr(replica, "replayed", down)
    assert replica.read(request("0/1"), lambda db: db, replica="default") == "default"
    assert routed == [("primary", "replica_error")]


def test_a_replica_further_behind_than_max_lag_isnt_read(routed: list[tuple[str, str]]) -> None:
    # The primary this far ahead: the replica hasn't replayed what it had max lag ago.
    ahead = replica.StalenessFloor(5, lambda: "FFFFFFFF/FFFFFFFF")
    assert replica.read(request(), lambda db: db, replica="default", floor=ahead) == "default"
    assert routed == [("primary", "stale")]


def test_positions_compare_as_numbers() -> None:
    assert replica.lsn("0/16B3740") == 0x16B3740
    assert replica.lsn("9/0") < replica.lsn("10/0")
    assert replica.lsn("0/FFFFFFFF") < replica.lsn("1/0")


class Clock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def test_the_floor_is_what_the_primary_had_up_to_max_lag_ago() -> None:
    clock, position = Clock(), [0x10]
    floor = replica.StalenessFloor(5, lambda: f"0/{position[0]:X}", clock)
    assert floor.floor() == "0/10"
    for second in range(1, 6):
        clock.now += 1
        position[0] = 0x10 + second
        floor.floor()
    assert floor.floor() == "0/10"
    clock.now += 1
    assert floor.floor() == "0/11"


def test_the_primary_is_asked_at_most_once_a_second() -> None:
    clock, asked = Clock(), []

    def primary() -> str:
        asked.append(1)
        return "0/10"

    floor = replica.StalenessFloor(5, primary, clock)
    for _ in range(100):
        clock.now += 0.005
        floor.floor()
    assert len(asked) == 1
    clock.now += 1
    floor.floor()
    assert len(asked) == 2


def test_after_a_quiet_spell_the_floor_is_the_newest_position() -> None:
    clock, position = Clock(), ["0/10"]
    floor = replica.StalenessFloor(5, lambda: position[0], clock)
    floor.floor()
    clock.now += 60
    position[0] = "0/30"
    assert floor.floor() == "0/30"
