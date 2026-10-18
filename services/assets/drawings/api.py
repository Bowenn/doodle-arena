"""The drawings REST API (design doc 6.3), through the gateway at /api/assets/.

Every endpoint needs an access token, verified here like everywhere else (ADR 0001). Approved
drawings are visible to every signed-in player; a drawing in any other state only to its
authors, and to anyone else it doesn't exist (404), so ids leak nothing. Images are presigned
GET URLs straight to object storage, fresh on every read.
"""

import functools
import logging
import uuid
from collections.abc import Callable
from typing import Any

from django.db.models import Q, QuerySet
from django.http import HttpRequest, HttpResponse, JsonResponse
from django.views.decorators.http import require_GET
from opentelemetry import trace

from config.authn import Claims, InvalidTokenError, bearer_token, verifier
from drawings import storage
from drawings.models import Drawing, Status

log = logging.getLogger(__name__)
DEFAULT_LIMIT, MAX_LIMIT = 20, 50

View = Callable[..., HttpResponse]


def error(request: HttpRequest, status: int, code: str, message: str) -> JsonResponse:
    """The shared error format (design doc 6.3)."""
    request_id = request.headers.get("X-Request-Id", "")
    return JsonResponse(
        {"error": {"code": code, "message": message, "request_id": request_id}}, status=status
    )


def authenticated(view: View) -> View:
    """Verifies the bearer token and passes its claims to the view."""

    @functools.wraps(view)
    def wrapper(request: HttpRequest, *args: Any, **kwargs: Any) -> HttpResponse:
        token = bearer_token(request.headers.get("Authorization", ""))
        if token is None:
            return error(request, 401, "UNAUTHENTICATED", "a bearer token is required")
        try:
            claims = verifier().verify(token)
        except InvalidTokenError as e:
            log.info("api request refused: invalid token", extra={"reason": str(e)})
            return error(request, 401, "UNAUTHENTICATED", "the access token is not valid")
        trace.get_current_span().set_attribute("enduser.id", claims.user_id)
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


@require_GET
@authenticated
def list_drawings(request: HttpRequest, claims: Claims) -> HttpResponse:
    """GET /api/assets/drawings?author_id=&match_id=&limit=&cursor=: approved drawings, newest
    first. With author_id, that author's drawings; with match_id, that match's. Either way the
    caller's own drawings are included in any state."""
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
    page = list(drawings.order_by("-id")[: limit + 1])
    more = len(page) > limit
    page = page[:limit]
    return JsonResponse(
        {
            "drawings": [represent(d) for d in page],
            "next_cursor": str(page[-1].id) if more else None,
        }
    )


@require_GET
@authenticated
def get_drawing(request: HttpRequest, claims: Claims, drawing_id: uuid.UUID) -> HttpResponse:
    drawing = visible(claims).filter(pk=drawing_id).first()
    if drawing is None:
        return error(request, 404, "DRAWING_NOT_FOUND", "no such drawing")
    return JsonResponse(represent(drawing))


@require_GET
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
