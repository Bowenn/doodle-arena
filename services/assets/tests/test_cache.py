"""The gallery cache (ADR 0024) against a real Redis: answers identical to Postgres's, only
approved drawings cached, an approval visible at once, the cache-aside race closed by the
generation, one fill under a stampede, and Postgres answering whenever Redis can't."""

import json
import threading
import time
import uuid
from collections.abc import Iterator
from typing import Any

import pytest
import redis
from django.test import Client

from drawings import cache, moderation
from drawings.models import DecidedBy, Drawing
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db
API = "/api/assets/drawings"


@pytest.fixture
def r(settings: Any, redis_url: str) -> Iterator[redis.Redis]:
    settings.ASSETS_CACHE = True
    settings.CACHE_REDIS_URL = redis_url
    client: redis.Redis = redis.Redis.from_url(redis_url)
    for key in client.scan_iter(cache.PREFIX + "*"):
        client.delete(key)
    cache.reset()
    yield client
    cache.reset()


@pytest.fixture
def as_user(signer: Any) -> Any:
    def headers(user_id: str) -> dict[str, str]:
        return {"Authorization": f"Bearer {signer.token(user_id, ['player'])}"}

    return headers


def approve(d: Drawing, django_capture_on_commit_callbacks: Any) -> Drawing:
    with django_capture_on_commit_callbacks(execute=True):
        moderation.decide(d.id, approve=True, by=DecidedBy.AI)
    d.refresh_from_db()
    return d


@pytest.fixture
def approved(make_drawing: Any, django_capture_on_commit_callbacks: Any) -> Any:
    count = iter(range(1, 10_000))

    def make(**fields: Any) -> Drawing:
        d = make_drawing(match_id=f"m_{next(count)}", **{**IMAGE, **fields})
        return approve(d, django_capture_on_commit_callbacks)

    return make


def gallery(client: Client, headers: dict[str, str], query: str = "") -> dict[str, Any]:
    resp = client.get(f"{API}?{query}", headers=headers)
    assert resp.status_code == 200
    body: dict[str, Any] = resp.json()
    return body


def test_a_cached_drawing_answers_like_postgres_with_urls_signed_on_the_way_out(
    client: Client, as_user: Any, approved: Any, r: redis.Redis
) -> None:
    d = approved()
    url = f"{API}/{d.id}"

    first = client.get(url, headers=as_user("u_x")).json()
    stored = json.loads(r.get(f"{cache.PREFIX}drawing:{d.id}") or b"{}")
    # The entry keeps the image keys, never a URL that would expire in the cache.
    assert stored["image_key"] == IMAGE["image_key"] and "image_url" not in stored
    # A change behind the cache's back proves the second answer came from it.
    Drawing.objects.filter(pk=d.id).update(word="changed")
    second = client.get(url, headers=as_user("u_y")).json()

    assert second == first
    assert "X-Amz-Signature=" in second["image_url"]


def test_only_approved_drawings_are_cached_and_an_authors_own_stays_theirs(
    client: Client, as_user: Any, make_drawing: Any, approved: Any, r: redis.Redis
) -> None:
    mine = make_drawing(match_id="m_mine", author_ids=["u_ada"])
    public = approved()

    assert client.get(f"{API}/{mine.id}", headers=as_user("u_ada")).status_code == 200
    assert not r.exists(f"{cache.PREFIX}drawing:{mine.id}")
    assert client.get(f"{API}/{mine.id}", headers=as_user("u_eve")).status_code == 404

    ids = f"{mine.id},{public.id}"
    as_author = client.get(f"{API}?ids={ids}", headers=as_user("u_ada")).json()["drawings"]
    as_other = client.get(f"{API}?ids={ids}", headers=as_user("u_eve")).json()["drawings"]
    assert [x["id"] for x in as_author] == [str(mine.id), str(public.id)]
    assert [x["id"] for x in as_other] == [str(public.id)]
    assert r.exists(f"{cache.PREFIX}drawing:{public.id}")
    assert not r.exists(f"{cache.PREFIX}drawing:{mine.id}")


def test_an_ids_lookup_mixes_cached_and_uncached_drawings_in_the_order_asked(
    client: Client, as_user: Any, approved: Any, r: redis.Redis
) -> None:
    a, b, c = approved(), approved(), approved()
    client.get(f"{API}/{b.id}", headers=as_user("u_x"))  # only b is cached

    body = client.get(f"{API}?ids={c.id},{b.id},{a.id}", headers=as_user("u_x")).json()

    assert [x["id"] for x in body["drawings"]] == [str(c.id), str(b.id), str(a.id)]
    assert all(r.exists(f"{cache.PREFIX}drawing:{x.id}") for x in (a, b, c))


def test_an_approval_shows_on_the_cached_gallery_at_once_and_a_rejection_bumps_nothing(
    client: Client,
    as_user: Any,
    make_drawing: Any,
    approved: Any,
    r: redis.Redis,
    django_capture_on_commit_callbacks: Any,
) -> None:
    old = approved()
    assert [x["id"] for x in gallery(client, as_user("u_x"))["drawings"]] == [str(old.id)]
    gen = int(r.get(cache.GEN_KEY) or 0)

    rejected = make_drawing(match_id="m_rejected", **IMAGE)
    with django_capture_on_commit_callbacks(execute=True):
        moderation.decide(rejected.id, approve=False, by=DecidedBy.AI)
    assert int(r.get(cache.GEN_KEY) or 0) == gen

    new = approved()
    assert int(r.get(cache.GEN_KEY) or 0) == gen + 1
    ids = [x["id"] for x in gallery(client, as_user("u_x"))["drawings"]]
    assert ids == [str(new.id), str(old.id)]


def test_a_fill_that_raced_an_approval_lands_in_a_generation_nobody_reads(
    approved: Any, r: redis.Redis, make_drawing: Any, django_capture_on_commit_callbacks: Any
) -> None:
    old = approved()
    late = make_drawing(match_id="m_late", **IMAGE)

    def fill_then_approval_commits(n: int) -> list[dict[str, Any]]:
        from drawings.api import _gallery_from_primary

        rows = _gallery_from_primary(n)  # read before the approval commits
        approve(late, django_capture_on_commit_callbacks)
        return rows

    stale = cache.gallery_page(20, None, fill_then_approval_commits)
    assert stale is not None and [x["id"] for x in stale["drawings"]] == [str(old.id)]

    from drawings.api import _gallery_from_primary

    fresh = cache.gallery_page(20, None, _gallery_from_primary)
    assert fresh is not None
    assert [x["id"] for x in fresh["drawings"]] == [str(late.id), str(old.id)]


def entries(n: int) -> list[dict[str, Any]]:
    return [{"id": str(uuid.uuid4()), "image_key": "", "thumb_key": ""} for _ in range(n)]


def test_a_stampede_on_a_cold_page_makes_one_fill(r: redis.Redis) -> None:
    rows = entries(5)
    fills = []

    def slow_fill(n: int) -> list[dict[str, Any]]:
        fills.append(n)
        time.sleep(0.05)
        return rows

    results: list[Any] = []
    start = threading.Barrier(50)

    def one() -> None:
        start.wait()
        results.append(cache.gallery_page(20, None, slow_fill))

    threads = [threading.Thread(target=one) for _ in range(50)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert fills == [3 * 20 + 1]
    assert len(results) == 50
    assert all(page == {"drawings": rows, "next_cursor": None} for page in results)


def test_a_waiter_whose_filler_died_reads_postgres_after_the_wait(
    client: Client, as_user: Any, approved: Any, r: redis.Redis
) -> None:
    d = approved()
    gen = int(r.get(cache.GEN_KEY) or 0)
    r.set(f"{cache.PREFIX}lock:gallery:{gen}:20:first", "someone-else", px=60_000)

    started = time.monotonic()
    assert cache.gallery_page(20, None, lambda n: pytest.fail("not the filler")) is None
    assert time.monotonic() - started >= cache.LOCK_WAIT

    assert [x["id"] for x in gallery(client, as_user("u_x"))["drawings"]] == [str(d.id)]


def test_the_first_pages_are_filled_at_once_and_deeper_pages_and_filters_read_postgres(
    client: Client, as_user: Any, approved: Any, r: redis.Redis
) -> None:
    made = [approved() for _ in range(8)]
    newest = [str(d.id) for d in reversed(made)]
    me = as_user("u_x")

    seen, cursor = [], ""
    for _ in range(4):  # pages of 2: three from the cache, the fourth from Postgres
        page = gallery(client, me, f"limit=2{cursor}")
        seen += [x["id"] for x in page["drawings"]]
        cursor = f"&cursor={page['next_cursor']}" if page["next_cursor"] else ""
    assert seen == newest

    pages = [k.decode() for k in r.scan_iter(cache.PREFIX + "gallery:*:2:*")]
    assert len(pages) == 3
    by_author = gallery(client, me, "author_id=u_ada")
    assert len(by_author["drawings"]) == 8
    assert not list(r.scan_iter(cache.PREFIX + "gallery:*:20:*"))


def test_invalidate_drawing_removes_its_entry(
    client: Client, as_user: Any, approved: Any, r: redis.Redis
) -> None:
    d = approved()
    client.get(f"{API}/{d.id}", headers=as_user("u_x"))
    assert r.exists(f"{cache.PREFIX}drawing:{d.id}")

    cache.invalidate_drawing(d.id)

    assert not r.exists(f"{cache.PREFIX}drawing:{d.id}")


def test_postgres_answers_when_redis_is_down_and_the_cache_steps_aside(
    client: Client, as_user: Any, approved: Any, settings: Any, caplog: Any
) -> None:
    d = approved()
    settings.ASSETS_CACHE = True
    settings.CACHE_REDIS_URL = "redis://127.0.0.1:1/1"  # nothing listens there
    cache.reset()
    try:
        assert client.get(f"{API}/{d.id}", headers=as_user("u_x")).status_code == 200
        assert "cache unavailable" in caplog.text
        assert cache._skip_until > time.monotonic()
        # Skipped now: no more attempts until SKIP_AFTER_ERROR has passed.
        caplog.clear()
        assert [x["id"] for x in gallery(client, as_user("u_x"))["drawings"]] == [str(d.id)]
        assert "cache unavailable" not in caplog.text
    finally:
        cache.reset()


def test_with_the_cache_off_redis_is_never_touched(
    client: Client, as_user: Any, approved: Any, settings: Any
) -> None:
    d = approved()
    settings.ASSETS_CACHE = False
    settings.CACHE_REDIS_URL = "redis://127.0.0.1:1/1"
    cache.reset()

    assert client.get(f"{API}/{d.id}", headers=as_user("u_x")).status_code == 200
    assert [x["id"] for x in gallery(client, as_user("u_x"))["drawings"]] == [str(d.id)]
    assert cache._skip_until == 0.0
