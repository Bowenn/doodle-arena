"""The Django admin's sign-in: Google sign-in bridged into a short Django session (ADR 0001).

The admin's login page sends the browser to the web app's /moderate route. The web app, which
holds the access token in memory, asks GET /admin/sso/ for a CSRF token, then calls
POST /admin/sso/ once with the token in the Authorization header (never in a URL) and the CSRF
token in X-CSRFToken. This view verifies the token like every other endpoint, requires the
moderator or admin role, syncs a Django user (no password, no email) and logs it in for at most
an hour. Django's CSRF protection stays on: the POST needs the token, and the csrftoken cookie
is scoped to /admin/.
"""

import logging
from datetime import timedelta
from urllib.parse import urlencode

from django.conf import settings
from django.contrib.auth import login, logout
from django.contrib.auth.models import Group, Permission, User
from django.db import transaction
from django.http import HttpRequest, HttpResponse, HttpResponseRedirect, JsonResponse
from django.middleware.csrf import get_token
from django.utils import timezone
from django.views.decorators.cache import never_cache
from django.views.decorators.http import require_GET, require_http_methods
from opentelemetry import metrics, trace

from config.authn import Claims, InvalidTokenError, bearer_token, verifier

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
sign_ins = metrics.get_meter(__name__).create_counter(
    "assets.admin.sign_ins", description="Moderator sign-ins to the Django admin, by result"
)

MODERATORS_GROUP = "moderators"
# What a moderator may do: see drawings and the queue, and approve or reject what's in it.
MODERATOR_PERMISSIONS = ["view_drawing", "view_reviewqueue", "moderate_drawing"]
DEFAULT_NEXT = "/admin/drawings/reviewqueue/"


def _error(request: HttpRequest, status: int, code: str, message: str) -> JsonResponse:
    """The shared error format (design doc 6.3)."""
    request_id = request.headers.get("X-Request-Id", "")
    body = {"error": {"code": code, "message": message, "request_id": request_id}}
    return JsonResponse(body, status=status)


def _safe_next(value: str | None) -> str:
    """Only admin pages, so the sign-in can't be turned into an open redirect."""
    if value and value.startswith("/admin/") and not value.startswith("/admin/sso/"):
        return value
    return DEFAULT_NEXT


def _moderators_group() -> Group:
    group, _ = Group.objects.get_or_create(name=MODERATORS_GROUP)
    perms = Permission.objects.filter(
        content_type__app_label="drawings", codename__in=MODERATOR_PERMISSIONS
    )
    group.permissions.set(perms)
    return group


@transaction.atomic
def sync_user(claims: Claims) -> User:
    """Creates or updates the Django user for an auth user from their current roles. The
    username is the auth user id; a user who lost the role loses staff access here."""
    user, _ = User.objects.select_for_update().get_or_create(username=claims.user_id)
    is_admin = "admin" in claims.roles
    is_moderator = is_admin or "moderator" in claims.roles
    user.first_name = claims.name[:150]
    user.is_active = True
    user.is_staff = is_moderator
    user.is_superuser = is_admin
    user.set_unusable_password()
    user.save()
    group = _moderators_group()
    if is_moderator:
        user.groups.add(group)
    else:
        user.groups.remove(group)
    return user


@never_cache
@require_http_methods(["GET", "POST"])
def sso(request: HttpRequest) -> HttpResponse:
    if request.method == "GET":
        # A page on another origin can't read this response, so only the web app gets the token.
        return JsonResponse({"csrf_token": get_token(request)})

    with tracer.start_as_current_span("admin.sso") as span:
        token = bearer_token(request.headers.get("Authorization", ""))
        if token is None:
            sign_ins.add(1, {"result": "unauthenticated"})
            span.set_attribute("sso.result", "unauthenticated")
            return _error(request, 401, "UNAUTHENTICATED", "a bearer token is required")
        try:
            claims = verifier().verify(token)
        except InvalidTokenError as e:
            log.warning("admin sign-in refused: invalid token", extra={"reason": str(e)})
            sign_ins.add(1, {"result": "unauthenticated"})
            span.set_attribute("sso.result", "unauthenticated")
            return _error(request, 401, "UNAUTHENTICATED", "the access token is not valid")

        user = sync_user(claims)
        if not user.is_staff:
            logout(request)  # ends any earlier admin session in this browser
            log.warning(
                "admin sign-in refused: no moderator role", extra={"user_id": user.username}
            )
            sign_ins.add(1, {"result": "forbidden"})
            span.set_attribute("sso.result", "forbidden")
            return _error(request, 403, "FORBIDDEN", "the moderator or admin role is required")

        login(request, user, backend="django.contrib.auth.backends.ModelBackend")
        # Absolute expiry: an hour after sign-in, however busy the session is, so a revoked role
        # stops working within the hour.
        request.session.set_expiry(timezone.now() + timedelta(seconds=settings.SESSION_COOKIE_AGE))
        log.info("moderator signed in to the admin", extra={"user_id": user.username})
        sign_ins.add(1, {"result": "ok"})
        span.set_attribute("sso.result", "ok")
        return JsonResponse({"next": _safe_next(request.GET.get("next"))})


@require_GET
def admin_login(request: HttpRequest) -> HttpResponse:
    """Replaces the admin's password form: send the browser to the web app's handoff, which
    comes back to `next` once signed in."""
    target = _safe_next(request.GET.get("next"))
    if request.user.is_active and request.user.is_staff:
        return HttpResponseRedirect(target)
    return HttpResponseRedirect(f"{settings.ADMIN_SIGN_IN_URL}?{urlencode({'next': target})}")
