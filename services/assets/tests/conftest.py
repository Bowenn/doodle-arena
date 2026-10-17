"""Shared fixtures. Database tests run against a real Postgres started by Testcontainers (never
an in-memory substitute), migrated with the same Django migrations as production; the Celery
tests use a real Redis broker."""

import os
import time
from collections.abc import Iterator
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

# Docker Desktop may not create /var/run/docker.sock; scripts/test.sh sets DOCKER_HOST from the
# current Docker context, and this fallback covers running pytest directly.
_desktop_socket = Path.home() / ".docker" / "run" / "docker.sock"
if "DOCKER_HOST" not in os.environ and _desktop_socket.exists():
    os.environ["DOCKER_HOST"] = f"unix://{_desktop_socket}"
    os.environ.setdefault("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock")

from testcontainers.community.postgres import PostgresContainer  # noqa: E402
from testcontainers.community.redis import RedisContainer  # noqa: E402

# The same images as infra/docker-compose.yml.
POSTGRES_IMAGE = "postgres:18.6-trixie"
REDIS_IMAGE = "redis:8.10.2-trixie"


@pytest.fixture(scope="session")
def postgres() -> Iterator[PostgresContainer]:
    with PostgresContainer(POSTGRES_IMAGE, driver=None) as pg:
        yield pg


@pytest.fixture(scope="session")
def django_db_modify_db_settings(postgres: PostgresContainer) -> None:
    """pytest-django calls this before it creates the test database: point Django at the
    container. Tests that never touch the database never start it."""
    from django.conf import settings

    settings.DATABASES["default"].update(
        HOST=postgres.get_container_host_ip(),
        PORT=int(postgres.get_exposed_port(5432)),
        NAME=postgres.dbname,
        USER=postgres.username,
        PASSWORD=postgres.password,
        CONN_MAX_AGE=0,
    )


@pytest.fixture(scope="session")
def redis_url() -> Iterator[str]:
    with RedisContainer(REDIS_IMAGE) as r:
        yield f"redis://{r.get_container_host_ip()}:{r.get_exposed_port(6379)}/1"


def round_fields(**overrides: Any) -> dict[str, Any]:
    """The fields a RoundFinished event gives a new drawing."""
    fields: dict[str, Any] = {
        "match_id": "m_1",
        "round_no": 1,
        "room_id": "r_1",
        "word": "cat",
        "author_ids": ["u_ada", "u_bob"],
        "initial_owner_id": "u_ada",
        "stroke_log_key": "m_1/1.binpb.gz",
        "stroke_count": 12,
        "canvas_width": 800,
        "canvas_height": 600,
        "ended_at": datetime(2026, 10, 17, 12, 0, tzinfo=UTC),
        "source_event_id": "6a1f0e1c-0000-7000-8000-000000000001",
    }
    fields.update(overrides)
    return fields


@pytest.fixture
def make_drawing() -> Any:
    from drawings.models import Drawing

    def make(**overrides: Any) -> Drawing:
        return Drawing.objects.create(**round_fields(**overrides))

    return make


class Signer:
    """Signs access tokens like auth does, with a throwaway key the verifier trusts."""

    issuer = "http://localhost:8080/api/auth"

    def __init__(self) -> None:
        from cryptography.hazmat.primitives.asymmetric import rsa

        self.private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)

    def token(self, sub: str, roles: list[str], ttl: int = 900, name: str = "Mo") -> str:
        import jwt

        now = int(time.time())
        claims = {
            "sub": sub,
            "roles": roles,
            "name": name,
            "iss": self.issuer,
            "aud": "doodle-api",
            "iat": now,
            "exp": now + ttl,
        }
        return jwt.encode(claims, self.private_key, "RS256", headers={"kid": "test-key"})


@pytest.fixture(scope="session")
def signer() -> Signer:
    return Signer()


@pytest.fixture(autouse=True)
def trusted_test_key(signer: Signer) -> Iterator[None]:
    """Every test's verifier trusts the signer's key, so no test ever fetches a JWKS."""
    from config import authn

    keys = authn.StaticKeys({"test-key": signer.private_key.public_key()})
    authn.set_verifier_for_tests(authn.TokenVerifier(keys, Signer.issuer))
    yield
    authn.set_verifier_for_tests(None)
