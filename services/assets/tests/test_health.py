from django.test import Client


def test_healthz_returns_service_identity(client: Client) -> None:
    response = client.get("/healthz")

    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "ok"
    assert body["service"] == "assets"
    assert body["version"]
    assert body["instance"]


def test_healthz_rejects_post(client: Client) -> None:
    assert client.post("/healthz").status_code == 405
