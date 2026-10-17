from django.contrib import admin
from django.http import HttpRequest, JsonResponse
from django.urls import path
from django.views.decorators.http import require_GET

from config import SERVICE_NAME
from config.settings import env
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
]
