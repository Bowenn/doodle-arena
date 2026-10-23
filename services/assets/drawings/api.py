"""The drawings REST API (design doc 6.3), through the gateway at /api/assets/.

Every endpoint needs an access token, verified here like everywhere else (ADR 0001). Approved
drawings are visible to every signed-in player; a drawing in any other state only to its
authors, and to anyone else it doesn't exist (404), so ids leak nothing. Images are presigned
GET URLs straight to object storage, fresh on every read.
"""

import functools
import logging
import time
import uuid
from collections.abc import Callable
from typing import Any

from django.db.models import Q, QuerySet
from django.http import HttpRequest, HttpResponse, JsonResponse
from django.views.decorators.csrf import csrf_exempt
from django.views.defaults import page_not_found as django_page_not_found
from django.views.defaults import server_error as django_server_error
from elastic_transport import ConnectionError as ESConnectionError
from elastic_transport import ConnectionTimeout, TransportError
from elasticsearch import ApiError
from opentelemetry import metrics, trace

from config.authn import Claims, InvalidTokenError, bearer_token, verifier
from drawings import replica, search, storage
from drawings.models import Drawing, SearchListing, Status

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
# Elasticsearch unreachable, timing out or refusing: search answers 503.
SEARCH_DOWN = (ESConnectionError, ConnectionTimeout, TransportError, ApiError)
DEFAULT_LIMIT, MAX_LIMIT = 20, 50

View = Callable[..., HttpResponse]


def error(request: HttpRequest, status: int, code: str, message: str) -> JsonResponse:
    """The shared error format (design doc 6.3)."""
    request_id = request.headers.get("X-Request-Id", "")
    return JsonResponse(
        {"error": {"code": code, "message": message, "request_id": request_id}}, status=status
    )


def authenticated(view: View) -> View:
    """A read-only API endpoint: GET only, the bearer token verified, its claims passed to the
    view. Exempt from CSRF: the API authenticates with the Authorization header, never a cookie,
    so a cross-site page can't make a request that carries the user's identity."""

    @csrf_exempt
    @functools.wraps(view)
    def wrapper(request: HttpRequest, *args: Any, **kwargs: Any) -> HttpResponse:
        if request.method not in ("GET", "HEAD"):
            refused = error(request, 405, "METHOD_NOT_ALLOWED", "only GET is allowed here")
            refused["Allow"] = "GET, HEAD"
            return refused
        token = bearer_token(request.headers.get("Authorization", ""))
        if token is None:
            return error(request, 401, "UNAUTHENTICATED", "a bearer token is required")
        try:
            claims = verifier().verify(token)
        except InvalidTokenError as e:
            log.info("api request refused: invalid token", extra={"reason": str(e)})
            return error(request, 401, "UNAUTHENTICATED", "the access token is not valid")
        trace.get_current_span().set_attribute("user.id", claims.user_id)
        response = view(request, claims, *args, **kwargs)
        # Presigned URLs expire, and authors see drawings others don't: per user, briefly.
        response["Cache-Control"] = "private, max-age=60"
        return response

    return wrapper


def visible(claims: Claims) -> QuerySet[Drawing]:
    return Drawing.objects.filter(
        Q(status=Status.APPROVED) | Q(author_ids__contains=[claims.user_id])
    )


def represent(d: Drawing) -> dict[str, Any]:
    body: dict[str, Any] = {
        "id": str(d.id),
        "match_id": d.match_id,
        "round_no": d.round_no,
        "word": d.word,  # safe: the round is over (RoundFinished publishes it too)
        "author_ids": d.author_ids,
        "initial_owner_id": d.initial_owner_id,
        "status": d.status,
        "ended_at": d.ended_at.isoformat(),
        "created_at": d.created_at.isoformat(),
        "has_replay": bool(d.replay_id),
        "image_url": None,
        "thumb_url": None,
    }
    if d.image_key:
        body["image_url"] = storage.presigned_url(d.image_key)
        body["thumb_url"] = storage.presigned_url(d.thumb_key)
    return body


def _uuid(value: str | None) -> uuid.UUID | None:
    try:
        return uuid.UUID(value) if value else None
    except ValueError:
        return None


MAX_IDS = 50


def lookup_drawings(request: HttpRequest, claims: Claims, raw: str) -> HttpResponse:
    """?ids=<up to 50 comma-separated ids>: those drawings the caller may see, in the order
    asked (the marketplace's pages show listings and owned drawings, which carry only ids)."""
    parts = [p for p in raw.split(",") if p]
    parsed = [_uuid(p) for p in parts]
    ids = [i for i in parsed if i is not None]
    if not parts or len(parts) > MAX_IDS or len(ids) != len(parts):
        return error(request, 400, "INVALID_REQUEST", f"ids must be 1 to {MAX_IDS} drawing ids")
    found = replica.read(
        request, lambda db: {d.id: d for d in visible(claims).using(db).filter(id__in=ids)}
    )
    ordered = [found[i] for i in dict.fromkeys(ids) if i in found]
    return JsonResponse({"drawings": [represent(d) for d in ordered], "next_cursor": None})


@authenticated
def list_drawings(request: HttpRequest, claims: Claims) -> HttpResponse:
    """GET /api/assets/drawings?author_id=&match_id=&limit=&cursor=: approved drawings, newest
    first. With author_id, that author's drawings; with match_id, that match's. Either way the
    caller's own drawings are included in any state. With ids=, a lookup instead (above)."""
    if "ids" in request.GET:
        return lookup_drawings(request, claims, request.GET["ids"])
    try:
        limit = int(request.GET.get("limit", DEFAULT_LIMIT))
    except ValueError:
        limit = 0
    if not 1 <= limit <= MAX_LIMIT:
        return error(request, 400, "INVALID_REQUEST", f"limit must be 1 to {MAX_LIMIT}")
    cursor_raw = request.GET.get("cursor")
    cursor = _uuid(cursor_raw)
    if cursor_raw and cursor is None:
        return error(request, 400, "INVALID_REQUEST", "bad cursor")
    author = request.GET.get("author_id", "")
    match = request.GET.get("match_id", "")
    if len(author) > 64 or len(match) > 64:
        return error(request, 400, "INVALID_REQUEST", "bad author_id or match_id")

    filtered = bool(author or match)
    drawings = visible(claims) if filtered else Drawing.objects.filter(status=Status.APPROVED)
    if author:
        drawings = drawings.filter(author_ids__contains=[author])
    if match:
        drawings = drawings.filter(match_id=match)  # the (match_id, round_no) unique index
    if cursor is not None:
        drawings = drawings.filter(id__lt=cursor)  # keyset: UUIDv7 ids sort by creation time
    # The replica, unless it hasn't replayed the caller's last write yet (ADR 0020).
    page = replica.read(request, lambda db: list(drawings.using(db).order_by("-id")[: limit + 1]))
    more = len(page) > limit
    page = page[:limit]
    return JsonResponse(
        {
            "drawings": [represent(d) for d in page],
            "next_cursor": str(page[-1].id) if more else None,
        }
    )


@authenticated
def get_drawing(request: HttpRequest, claims: Claims, drawing_id: uuid.UUID) -> HttpResponse:
    drawing = replica.read(
        request, lambda db: visible(claims).using(db).filter(pk=drawing_id).first()
    )
    if drawing is None:
        return error(request, 404, "DRAWING_NOT_FOUND", "no such drawing")
    return JsonResponse(represent(drawing))


@authenticated
def get_replay(request: HttpRequest, claims: Claims, drawing_id: uuid.UUID) -> HttpResponse:
    """The stored replay: every stroke with its points as [x, y, t_ms], undone strokes flagged."""
    drawing = visible(claims).filter(pk=drawing_id).first()
    doc = storage.get_replay(drawing.replay_id) if drawing and drawing.replay_id else None
    if doc is None:
        return error(request, 404, "REPLAY_NOT_FOUND", "no replay for this drawing")
    doc.pop("_id", None)
    started = doc.get("round_started_at")
    if started is not None:
        doc["round_started_at"] = started.isoformat()
    return JsonResponse({"drawing_id": str(drawing_id), **doc})


def not_found(request: HttpRequest, exception: Exception | None = None) -> HttpResponse:
    """handler404: the shared JSON error for API paths, Django's page elsewhere (the admin)."""
    if request.path.startswith("/api/"):
        return error(request, 404, "NOT_FOUND", "no such resource")
    return django_page_not_found(request, exception or Exception())


def server_error(request: HttpRequest) -> HttpResponse:
    """handler500: the shared JSON error for API paths. The exception itself is in the logs."""
    if request.path.startswith("/api/"):
        return error(request, 500, "INTERNAL", "something went wrong; try again")
    return django_server_error(request)


search_requests = metrics.get_meter(__name__).create_counter(
    "assets.search.requests",
    description="Search requests by result: ok, invalid (bad parameters), unavailable "
    "(no Elasticsearch)",
)
search_duration = metrics.get_meter(__name__).create_histogram(
    "assets.search.duration", unit="ms", description="One search, Elasticsearch and hydration"
)


def _whole(request: HttpRequest, name: str, low: int, high: int) -> int | None:
    raw = request.GET.get(name)
    if raw is None or raw == "":
        return None
    if not raw.isdigit() or not low <= int(raw) <= high:
        raise ValueError(f"{name} must be a whole number from {low} to {high}")
    return int(raw)


def _query(request: HttpRequest) -> search.Query:
    """The search parameters, checked; ValueError names the first bad one."""
    q = request.GET.get("q", "").strip()
    if len(q) > 64:
        raise ValueError("q is at most 64 characters")
    sort = request.GET.get("sort", "relevance")
    if sort not in search.SORTS:
        raise ValueError("sort is one of " + ", ".join(search.SORTS))
    listed = request.GET.get("listed", "")
    if listed not in ("", "true", "false"):
        raise ValueError("listed is true or false")
    author = request.GET.get("author_id", "")
    if len(author) > 64:
        raise ValueError("author_id is at most 64 characters")
    cursor = request.GET.get("cursor")
    return search.Query(
        q=q,
        listed=listed == "true",
        min_price=_whole(request, "min_price", 1, 1_000_000),
        max_price=_whole(request, "max_price", 1, 1_000_000),
        author_id=author,
        sort=sort,
        limit=_whole(request, "limit", 1, MAX_LIMIT) or DEFAULT_LIMIT,
        after=search.decode_cursor(cursor) if cursor else None,
    )


@authenticated
def search_drawings(request: HttpRequest, claims: Claims) -> HttpResponse:
    """GET /api/assets/search?q=&listed=&min_price=&max_price=&author_id=&sort=&limit=&cursor=
    (ADR 0023). Elasticsearch ranks; the hits are hydrated from Postgres, which re-checks that
    each drawing is approved and signs fresh image URLs, with the listing assets knows of."""
    started = time.monotonic()
    try:
        query = _query(request)
    except ValueError as e:
        search_requests.add(1, {"result": "invalid"})
        return error(request, 400, "INVALID_REQUEST", str(e))
    with tracer.start_as_current_span("search.query") as span:
        span.set_attribute("search.sort", query.sort)
        span.set_attribute("search.listed", query.listed)
        try:
            hits = search.search(query)
        except (search.SearchUnavailable, *SEARCH_DOWN) as e:
            log.warning("search unavailable", extra={"error": str(e)[:300]})
            search_requests.add(1, {"result": "unavailable"})
            return error(request, 503, "SEARCH_UNAVAILABLE", "search is unavailable right now")
        span.set_attribute("search.hits", len(hits.ids))
        rows = {d.id: d for d in Drawing.objects.filter(id__in=hits.ids, status=Status.APPROVED)}
        listings = {
            row.drawing_id: row for row in SearchListing.objects.filter(drawing_id__in=hits.ids)
        }
    drawings = []
    for drawing_id in hits.ids:
        d = rows.get(drawing_id)
        if d is None:
            continue  # rejected (or gone) since it was indexed: Postgres is the truth
        listing = listings.get(drawing_id)
        drawings.append(
            {
                **represent(d),
                "listing": None
                if listing is None
                else {
                    "listing_id": str(listing.listing_id),
                    "price": listing.price,
                    "seller_id": listing.seller_id,
                },
            }
        )
    search_requests.add(1, {"result": "ok"})
    search_duration.record((time.monotonic() - started) * 1000)
    return JsonResponse(
        {
            "drawings": drawings,
            "total": hits.total,
            "facets": {"price": hits.facets},
            "next_cursor": search.encode_cursor(hits.next_after) if hits.next_after else None,
        }
    )
