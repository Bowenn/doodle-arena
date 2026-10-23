from django.contrib import admin
from django.http import HttpRequest, JsonResponse
from django.urls import path
from django.views.decorators.http import require_GET

from config import SERVICE_NAME
from config.settings import env
from drawings import api
from sso import views as sso_views


@require_GET
def healthz(request: HttpRequest) -> JsonResponse:
    return JsonResponse(
        {
            "status": "ok",
            "service": SERVICE_NAME,
            "version": env.service_version,
            "instance": env.service_instance_id,
        }
    )


urlpatterns = [
    path("healthz", healthz),
    # Before admin.site.urls, so they replace the admin's password login (ADR 0001).
    path("admin/login/", sso_views.admin_login),
    path("admin/sso/", sso_views.sso),
    path("admin/", admin.site.urls),
    path("api/assets/drawings", api.list_drawings),
    path("api/assets/search", api.search_drawings),
    path("api/assets/drawings/<uuid:drawing_id>", api.get_drawing),
    path("api/assets/drawings/<uuid:drawing_id>/replay", api.get_replay),
]

# The shared JSON error format (design doc 6.3) for API paths; Django's pages for the admin.
handler404 = "drawings.api.not_found"
handler500 = "drawings.api.server_error"
