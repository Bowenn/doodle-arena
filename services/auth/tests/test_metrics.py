"""auth.logins and auth.refreshes count the right result on every path (docs/observability.md)."""

import logging

import httpx
import pytest
from fastapi.testclient import TestClient
from opentelemetry.sdk.metrics.export import InMemoryMetricReader, NumberDataPoint
from sqlalchemy import Engine, text

from tests.fake_google import FakeGoogle
from tests.test_google_login import finish_login, start_login
from tests.test_refresh import ORIGIN, current_cookie, login, use


def counts(reader: InMemoryMetricReader, name: str) -> dict[str, int]:
    """The counter's value per `result` attribute."""
    result: dict[str, int] = {}
    data = reader.get_metrics_data()
    for rm in data.resource_metrics if data else []:
        for sm in rm.scope_metrics:
            for metric in sm.metrics:
                if metric.name != name:
                    continue
                for point in metric.data.data_points:
                    assert isinstance(point, NumberDataPoint)
                    attrs = dict(point.attributes or {})
                    assert set(attrs) == {"result"}  # never an ID
                    result[str(attrs["result"])] = int(point.value)
    return result


def test_successful_login_counts_success(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    login(client, google)

    assert counts(metric_reader, "auth.logins") == {"success": 1}


def test_state_mismatch_counts_failed(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    params = start_login(client)
    google.answer_with(google.id_token(nonce=params["nonce"]))

    assert finish_login(client, state="attacker-state").status_code == 400
    assert counts(metric_reader, "auth.logins") == {"failed": 1}


def test_missing_state_cookie_counts_failed(
    client: TestClient, metric_reader: InMemoryMetricReader
) -> None:
    params = start_login(client)
    client.cookies.clear()

    assert finish_login(client, params["state"]).status_code == 400
    assert counts(metric_reader, "auth.logins") == {"failed": 1}


def test_google_outage_counts_failed(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    params = start_login(client)
    google.fail_with(httpx.ConnectTimeout("timed out"))

    assert finish_login(client, params["state"]).status_code == 502
    assert counts(metric_reader, "auth.logins") == {"failed": 1}


def test_bad_id_token_counts_failed(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    params = start_login(client)
    google.answer_with(google.id_token(nonce="replayed-nonce"))

    assert finish_login(client, params["state"]).status_code == 400
    assert counts(metric_reader, "auth.logins") == {"failed": 1}


def test_cancelled_login_is_not_counted(
    client: TestClient, metric_reader: InMemoryMetricReader
) -> None:
    start_login(client)
    client.get("/api/auth/google/callback", params={"error": "access_denied"})

    assert counts(metric_reader, "auth.logins") == {}


def test_rotation_counts_rotated(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    login(client, google)
    assert client.post("/api/auth/refresh", headers=ORIGIN).status_code == 200
    assert client.post("/api/auth/refresh", headers=ORIGIN).status_code == 200

    assert counts(metric_reader, "auth.refreshes") == {"rotated": 2}


def test_reuse_counts_reuse_detected(
    client: TestClient, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> None:
    stolen = login(client, google)
    assert use(client, stolen) == 200
    legit = current_cookie(client)
    assert legit

    assert use(client, stolen) == 401  # reuse: the family is revoked
    assert use(client, legit) == 401  # a revoked token is reuse too

    assert counts(metric_reader, "auth.refreshes") == {"rotated": 1, "reuse_detected": 2}


def test_expired_token_counts_expired(
    client: TestClient, google: FakeGoogle, db: Engine, metric_reader: InMemoryMetricReader
) -> None:
    token = login(client, google)
    with db.begin() as conn:
        conn.execute(text("UPDATE refresh_tokens SET expires_at = now() - interval '1 second'"))

    assert use(client, token) == 401
    assert counts(metric_reader, "auth.refreshes") == {"expired": 1}


def test_unknown_token_counts_invalid(
    client: TestClient, metric_reader: InMemoryMetricReader
) -> None:
    assert use(client, "not-a-real-token") == 401

    assert counts(metric_reader, "auth.refreshes") == {"invalid": 1}


def test_missing_cookie_counts_invalid_and_logs(
    client: TestClient, metric_reader: InMemoryMetricReader, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.INFO, logger="auth"):
        assert client.post("/api/auth/refresh", headers=ORIGIN).status_code == 401

    assert counts(metric_reader, "auth.refreshes") == {"invalid": 1}
    [record] = [r for r in caplog.records if r.getMessage() == "refresh rejected"]
    assert record.levelno == logging.INFO
    assert record.__dict__["reason"] == "missing_cookie"
