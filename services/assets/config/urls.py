from django.http import HttpRequest, JsonResponse
from django.urls import path
from django.views.decorators.http import require_GET

from config.settings import SERVICE_NAME, env


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
]
