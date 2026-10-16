from fastapi.testclient import TestClient

from app.main import create_app
from app.settings import Settings


def test_healthz_returns_service_identity() -> None:
    settings = Settings(service_version="1.2.3", service_instance_id="ai-1")
    client = TestClient(create_app(settings))

    response = client.get("/healthz")

    assert response.status_code == 200
    assert response.json() == {
        "status": "ok",
        "service": "ai",
        "version": "1.2.3",
        "instance": "ai-1",
    }


def test_healthz_rejects_post() -> None:
    client = TestClient(create_app(Settings()))
    assert client.post("/healthz").status_code == 405
