from fastapi.testclient import TestClient

from app.main import create_app
from app.settings import Settings


def test_healthz_returns_service_identity(settings: Settings) -> None:
    settings.service_instance_id = "auth-1"
    client = TestClient(create_app(settings))

    response = client.get("/healthz")

    assert response.status_code == 200
    assert response.json() == {
        "status": "ok",
        "service": "auth",
        "version": "1.2.3",
        "instance": "auth-1",
    }


def test_errors_use_the_shared_format(settings: Settings) -> None:
    client = TestClient(create_app(settings))

    response = client.post("/healthz", headers={"X-Request-Id": "req-1"})

    assert response.status_code == 405
    assert response.json() == {
        "error": {
            "code": "METHOD_NOT_ALLOWED",
            "message": "Method Not Allowed",
            "request_id": "req-1",
        }
    }
