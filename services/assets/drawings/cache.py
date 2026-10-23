"""The gallery cache (ADR 0024): Redis cache-aside for approved drawings and the gallery's first
pages, under assets:cache:* in assets' own Redis database (the one Celery uses).

- Only approved drawings are cached: they are the same for every player and, since a decision is
  final, never change. An entry holds a drawing's fields with its image keys, never presigned
  URLs; the API signs those when it answers.
- Gallery pages carry a generation. An approval increments it after its commit; a reader reads
  the generation before its query and stores under the one it read, so a reader that raced an
  approval can only fill a generation nobody asks for any more.
- One request fills a missing page (a lock with a token); the others wait up to LOCK_WAIT for it,
  then read Postgres themselves.
- Redis failing never fails a request: the cache is skipped for SKIP_AFTER_ERROR and Postgres
  answers. ASSETS_CACHE=off turns it off.
"""

import contextlib
import json
import logging
import threading
import time
import uuid
from collections.abc import Callable, Iterable
from typing import Any

import redis
from django.conf import settings
from opentelemetry import metrics, trace

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
meter = metrics.get_meter(__name__)
requests = meter.create_counter(
    "assets.cache.requests",
    description="Cache lookups by cache (drawing, gallery) and result (hit, miss, error; off: "
    "ASSETS_CACHE=off; skipped: after a Redis error)",
)
lock_waits = meter.create_counter(
    "assets.cache.lock_waits",
    description="Requests that waited for another's gallery fill, by result (filled, timed_out)",
)
invalidations = meter.create_counter(
    "assets.cache.invalidations",
    description="Cache invalidations by cache (gallery: a generation bump; drawing: a delete)",
)

PREFIX = "assets:cache:"
DRAWING_TTL = 600
PAGE_TTL = 60
# Page 1's fill stores the first PAGES pages of the walk at once (one query).
PAGES = 3
GEN_KEY = PREFIX + "gallery:gen"
LOCK_TTL_MS = 2000
LOCK_WAIT = 0.2
LOCK_POLL = 0.01
TIMEOUT = 0.1
SKIP_AFTER_ERROR = 5.0

# Releases the lock only if it is still ours: a fill slower than LOCK_TTL_MS must not delete a
# lock another request has taken since.
_RELEASE = """
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
"""

_lock = threading.Lock()
_client: redis.Redis | None = None
_client_url = ""
_skip_until = 0.0


class Unavailable(Exception):
    """The cache is off, or Redis failed recently ("off" or "skipped"): read Postgres."""


def _redis() -> redis.Redis:
    """The client for the configured Redis, built on first use (after gunicorn forks)."""
    global _client, _client_url
    if not settings.ASSETS_CACHE:
        raise Unavailable("off")
    if time.monotonic() < _skip_until:
        raise Unavailable("skipped")
    url = settings.CACHE_REDIS_URL
    with _lock:
        if _client is None or _client_url != url:
            _client = redis.Redis.from_url(
                url,
                username=settings.CACHE_REDIS_USERNAME or None,
                password=settings.CACHE_REDIS_PASSWORD or None,
                socket_timeout=TIMEOUT,
                socket_connect_timeout=TIMEOUT,
            )
            _client_url = url
        return _client


def _failed(e: Exception, cache: str) -> None:
    """Counts a Redis error and skips the cache for a while, so an outage costs one timeout."""
    global _skip_until
    _skip_until = time.monotonic() + SKIP_AFTER_ERROR
    requests.add(1, {"cache": cache, "result": "error"})
    log.warning(
        "cache unavailable; reading Postgres",
        extra={"cache": cache, "error": str(e)[:300], "skip_s": SKIP_AFTER_ERROR},
    )


def reset() -> None:
    """Forgets the client and any recent error (tests)."""
    global _client, _skip_until
    with _lock:
        _client = None
        _skip_until = 0.0


# ---------------------------------------------------------------------------------- drawings


def _drawing_key(drawing_id: uuid.UUID) -> str:
    return f"{PREFIX}drawing:{drawing_id}"


def get_drawings(ids: Iterable[uuid.UUID]) -> dict[uuid.UUID, dict[str, Any]]:
    """The cached entries among ids (one MGET). Anything missing comes from Postgres."""
    ids = list(dict.fromkeys(ids))
    if not ids:
        return {}
    try:
        r = _redis()
    except Unavailable as e:
        requests.add(len(ids), {"cache": "drawing", "result": str(e)})
        return {}
    with tracer.start_as_current_span("cache.get", attributes={"cache.name": "drawing"}) as span:
        try:
            values = r.mget([_drawing_key(i) for i in ids])
        except redis.RedisError as e:
            _failed(e, "drawing")
            return {}
        found = {i: json.loads(v) for i, v in zip(ids, values, strict=True) if v is not None}
        span.set_attribute("cache.hit", len(found) == len(ids))
    if found:
        requests.add(len(found), {"cache": "drawing", "result": "hit"})
    if len(found) < len(ids):
        requests.add(len(ids) - len(found), {"cache": "drawing", "result": "miss"})
    return found


def put_drawings(entries: dict[uuid.UUID, dict[str, Any]]) -> None:
    """Stores approved drawings' entries (the caller checks they're approved)."""
    if not entries:
        return
    try:
        r = _redis()
        pipe = r.pipeline(transaction=False)
        for drawing_id, fields in entries.items():
            pipe.set(_drawing_key(drawing_id), json.dumps(fields), ex=DRAWING_TTL)
        pipe.execute()
    except Unavailable:
        return
    except redis.RedisError as e:
        _failed(e, "drawing")


def invalidate_drawing(drawing_id: uuid.UUID) -> None:
    """Removes a drawing's entry. Nothing calls it today, since an approval is final; a takedown
    (approved to rejected) must call it and gallery_changed() after its commit (ADR 0024)."""
    try:
        _redis().delete(_drawing_key(drawing_id))
        invalidations.add(1, {"cache": "drawing"})
    except Unavailable:
        return
    except redis.RedisError as e:
        _failed(e, "drawing")


# ---------------------------------------------------------------------------------- gallery


def gallery_changed() -> None:
    """A drawing was approved: every cached gallery page is out of date. Called after the commit
    (moderation.decide), so a reader that sees the new generation also sees the drawing."""
    try:
        _redis().incr(GEN_KEY)
        invalidations.add(1, {"cache": "gallery"})
    except Unavailable:
        return
    except redis.RedisError as e:
        # The pages expire within PAGE_TTL on their own.
        _failed(e, "gallery")


def _page_key(gen: int, limit: int, cursor: uuid.UUID | None) -> str:
    return f"{PREFIX}gallery:{gen}:{limit}:{cursor or 'first'}"


Page = dict[str, Any]  # {"drawings": [fields, ...], "next_cursor": str | None}


def gallery_page(
    limit: int,
    cursor: uuid.UUID | None,
    fill: Callable[[int], list[dict[str, Any]]],
) -> Page | None:
    """A gallery page from the cache, or None: read Postgres as usual. fill(n) returns the newest
    n approved drawings' entries from the primary; it runs at most once per miss of page 1, and
    fills the first PAGES pages at once. A missing later page isn't filled here."""
    try:
        r = _redis()
    except Unavailable as e:
        requests.add(1, {"cache": "gallery", "result": str(e)})
        return None
    with tracer.start_as_current_span("cache.get", attributes={"cache.name": "gallery"}) as span:
        try:
            gen = int(r.get(GEN_KEY) or 0)
            key = _page_key(gen, limit, cursor)
            cached = r.get(key)
        except redis.RedisError as e:
            _failed(e, "gallery")
            return None
        span.set_attribute("cache.hit", cached is not None)
    if cached is not None:
        requests.add(1, {"cache": "gallery", "result": "hit"})
        page: Page = json.loads(cached)
        return page
    requests.add(1, {"cache": "gallery", "result": "miss"})
    if cursor is not None:
        return None
    return _fill_first_pages(r, gen, limit, key, fill)


def _fill_first_pages(
    r: redis.Redis,
    gen: int,
    limit: int,
    key: str,
    fill: Callable[[int], list[dict[str, Any]]],
) -> Page | None:
    lock = PREFIX + "lock:" + key.removeprefix(PREFIX)
    token = uuid.uuid4().hex
    try:
        if not r.set(lock, token, nx=True, px=LOCK_TTL_MS):
            return _wait_for(r, key)
    except redis.RedisError as e:
        _failed(e, "gallery")
        return None
    try:
        with tracer.start_as_current_span("cache.fill", attributes={"cache.name": "gallery"}):
            pages = _split(fill(PAGES * limit + 1), limit)
            try:
                pipe = r.pipeline(transaction=False)
                cursor: uuid.UUID | None = None
                for page in pages:
                    pipe.set(_page_key(gen, limit, cursor), json.dumps(page), ex=PAGE_TTL)
                    cursor = uuid.UUID(page["next_cursor"]) if page["next_cursor"] else None
                pipe.execute()
            except redis.RedisError as e:
                _failed(e, "gallery")
        return pages[0]
    finally:
        with contextlib.suppress(redis.RedisError):  # else it expires after LOCK_TTL_MS
            r.eval(_RELEASE, 1, lock, token)


def _split(rows: list[dict[str, Any]], limit: int) -> list[Page]:
    """The first PAGES pages of a walk, from the newest PAGES * limit + 1 rows."""
    pages = []
    for start in range(0, PAGES * limit, limit):
        chunk = rows[start : start + limit]
        more = len(rows) > start + limit
        pages.append({"drawings": chunk, "next_cursor": chunk[-1]["id"] if more else None})
        if not more:
            break
    return pages


def _wait_for(r: redis.Redis, key: str) -> Page | None:
    """Another request holds the fill: wait up to LOCK_WAIT for its page, else None."""
    deadline = time.monotonic() + LOCK_WAIT
    while time.monotonic() < deadline:
        time.sleep(LOCK_POLL)
        try:
            cached = r.get(key)
        except redis.RedisError as e:
            _failed(e, "gallery")
            return None
        if cached is not None:
            lock_waits.add(1, {"result": "filled"})
            page: Page = json.loads(cached)
            return page
    lock_waits.add(1, {"result": "timed_out"})
    return None
