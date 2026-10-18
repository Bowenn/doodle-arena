"""API keys (design doc 4.2): created and listed by their owner with an access token, verified by
other services on the internal port only, stored as a hash."""

from datetime import UTC, datetime, timedelta
from typing import Any

from fastapi.testclient import TestClient
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from sqlalchemy import Engine, text

from app import api_keys
from tests.test_metrics import counts

INTERNAL = "http://localhost:9091"


def bearer(client: TestClient, user_id: str) -> dict[str, str]:
    ctx = client.app.state.ctx  # type: ignore[attr-defined]
    token, _ = ctx.issuer.issue(user_id, ["player"], datetime.now(UTC))
    return {"Authorization": f"Bearer {token}"}


def create(client: TestClient, user_id: str, name: str = "ci bot") -> dict[str, Any]:
    r = client.post(
        "/api/auth/api-keys",
        json={"name": name, "scopes": ["webhooks"]},
        headers=bearer(client, user_id),
    )
    assert r.status_code == 201, r.text
    body: dict[str, Any] = r.json()
    return body


def verify(client: TestClient, key: str) -> Any:
    return client.post(f"{INTERNAL}/internal/api-keys/verify", json={"key": key})


def test_create_returns_the_key_once_and_stores_only_a_hash(client: TestClient, db: Engine) -> None:
    created = create(client, "u-ada")

    key = created["key"]
    assert key.startswith(f"da_{created['prefix']}_")
    assert created["scopes"] == ["webhooks"]
    with db.connect() as conn:
        row = conn.execute(
            text("SELECT secret_hash FROM api_keys WHERE prefix = :p"), {"p": created["prefix"]}
        ).one()
    secret = key.split("_", 2)[2]
    assert bytes(row.secret_hash) == api_keys.hash_secret(secret)
    assert secret.encode() not in bytes(row.secret_hash)

    listed = client.get("/api/auth/api-keys", headers=bearer(client, "u-ada")).json()["api_keys"]
    assert [k["id"] for k in listed] == [created["id"]]
    assert "key" not in listed[0]


def test_keys_are_private_to_their_owner(client: TestClient) -> None:
    created = create(client, "u-ada")
    assert (
        client.get("/api/auth/api-keys", headers=bearer(client, "u-bob")).json()["api_keys"] == []
    )
    r = client.delete(f"/api/auth/api-keys/{created['id']}", headers=bearer(client, "u-bob"))
    assert r.status_code == 404
    assert verify(client, created["key"]).status_code == 200


def test_verify_answers_who_owns_the_key(
    client: TestClient, metric_reader: InMemoryMetricReader
) -> None:
    created = create(client, "u-ada")

    r = verify(client, created["key"])

    assert r.status_code == 200
    assert r.json() == {"key_id": created["id"], "user_id": "u-ada", "scopes": ["webhooks"]}


def test_wrong_unknown_or_malformed_keys_are_refused(client: TestClient) -> None:
    created = create(client, "u-ada")
    wrong_secret = created["key"][:-4] + ("AAAA" if not created["key"].endswith("AAAA") else "BBBB")
    for key in [wrong_secret, f"da_{'x' * 12}_secret", "da_short_x", "not-a-key", ""]:
        r = verify(client, key)
        assert r.status_code == 401, key
        assert r.json()["error"]["code"] == "API_KEY_INVALID"


def test_revoked_key_stops_working(client: TestClient) -> None:
    created = create(client, "u-ada")

    r = client.delete(f"/api/auth/api-keys/{created['id']}", headers=bearer(client, "u-ada"))

    assert r.status_code == 204
    assert verify(client, created["key"]).status_code == 401
    assert (
        client.get("/api/auth/api-keys", headers=bearer(client, "u-ada")).json()["api_keys"] == []
    )


def test_verify_answers_only_on_the_internal_port(client: TestClient) -> None:
    created = create(client, "u-ada")
    r = client.post("/internal/api-keys/verify", json={"key": created["key"]})
    assert r.status_code == 404


def test_creating_needs_an_access_token_and_a_known_scope(client: TestClient) -> None:
    r = client.post("/api/auth/api-keys", json={"name": "x", "scopes": ["webhooks"]})
    assert r.status_code == 401
    r = client.post(
        "/api/auth/api-keys",
        json={"name": "x", "scopes": ["admin"]},
        headers=bearer(client, "u-ada"),
    )
    assert r.status_code == 400
    assert r.json()["error"]["code"] == "INVALID_SCOPE"
    r = client.post(
        "/api/auth/api-keys",
        json={"name": "", "scopes": ["webhooks"]},
        headers=bearer(client, "u-ada"),
    )
    assert r.status_code == 400


def test_at_most_ten_active_keys(client: TestClient) -> None:
    keys = [create(client, "u-ada", f"key {i}") for i in range(api_keys.MAX_ACTIVE_KEYS)]
    r = client.post(
        "/api/auth/api-keys",
        json={"name": "one more", "scopes": ["webhooks"]},
        headers=bearer(client, "u-ada"),
    )
    assert r.status_code == 409
    client.delete(f"/api/auth/api-keys/{keys[0]['id']}", headers=bearer(client, "u-ada"))
    create(client, "u-ada", "after a revoke")


def test_last_used_is_updated_at_most_once_a_minute(client: TestClient, db: Engine) -> None:
    created = create(client, "u-ada")
    verify(client, created["key"])
    with db.connect() as conn:
        first = conn.execute(
            text("SELECT last_used_at FROM api_keys WHERE id = :i"), {"i": created["id"]}
        ).scalar_one()
    verify(client, created["key"])
    with db.connect() as conn:
        second = conn.execute(
            text("SELECT last_used_at FROM api_keys WHERE id = :i"), {"i": created["id"]}
        ).scalar_one()
    assert first is not None
    assert second == first
    with db.begin() as conn:
        conn.execute(
            text("UPDATE api_keys SET last_used_at = :t WHERE id = :i"),
            {"t": datetime.now(UTC) - timedelta(minutes=2), "i": created["id"]},
        )
    verify(client, created["key"])
    with db.connect() as conn:
        third = conn.execute(
            text("SELECT last_used_at FROM api_keys WHERE id = :i"), {"i": created["id"]}
        ).scalar_one()
    assert third > first


def test_verifications_are_counted(client: TestClient, metric_reader: InMemoryMetricReader) -> None:
    created = create(client, "u-ada")
    verify(client, created["key"])
    verify(client, "not-a-key")

    assert counts(metric_reader, "auth.api_keys.verifications") == {"valid": 1, "invalid": 1}
