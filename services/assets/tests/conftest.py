"""Shared fixtures. Database tests run against a real Postgres started by Testcontainers (never
an in-memory substitute), migrated with the same Django migrations as production; the Celery
tests use a real Redis broker."""

import json
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


MONGO_IMAGE = "mongo:8.0.34"
SEAWEEDFS_IMAGE = "chrislusf/seaweedfs:4.48"
S3_KEY, S3_SECRET = "testkey", "testsecret"


@pytest.fixture(scope="session")
def mongo_url() -> Iterator[str]:
    from testcontainers.community.mongodb import MongoDbContainer

    with MongoDbContainer(MONGO_IMAGE) as m:
        yield m.get_connection_url().rstrip("/") + "/assets?authSource=admin"


@pytest.fixture(scope="session")
def s3_endpoint() -> Iterator[str]:
    """SeaweedFS with an S3 gateway, configured like infra/seaweedfs/entrypoint.sh."""
    import boto3
    from testcontainers.core.container import DockerContainer

    config = json.dumps(
        {
            "identities": [
                {
                    "name": "test",
                    "credentials": [{"accessKey": S3_KEY, "secretKey": S3_SECRET}],
                    "actions": ["Admin", "Read", "List", "Tagging", "Write"],
                }
            ]
        }
    )
    container = (
        DockerContainer(SEAWEEDFS_IMAGE)
        .with_exposed_ports(8333)
        .with_kwargs(entrypoint="sh")
        .with_command(
            [
                "-c",
                f"echo '{config}' > /tmp/s3.json && exec weed server -dir=/data "
                "-ip.bind=0.0.0.0 -volume.max=0 -s3 -s3.port=8333 -s3.config=/tmp/s3.json",
            ]
        )
    )
    with container:
        endpoint = f"http://{container.get_container_host_ip()}:{container.get_exposed_port(8333)}"
        client = boto3.client(
            "s3",
            endpoint_url=endpoint,
            region_name="us-east-1",
            aws_access_key_id=S3_KEY,
            aws_secret_access_key=S3_SECRET,
        )
        deadline = time.monotonic() + 60
        while True:  # the S3 gateway answers a few seconds after the port opens
            try:
                for bucket in ("round-logs", "drawings"):
                    client.create_bucket(Bucket=bucket)
                break
            except Exception:
                if time.monotonic() > deadline:
                    raise
                time.sleep(0.5)
        yield endpoint


@pytest.fixture
def stores(settings: Any, s3_endpoint: str, mongo_url: str) -> Iterator[None]:
    """Points assets at the test containers, with an empty replays collection."""
    from drawings import storage

    settings.S3_ENDPOINT = s3_endpoint
    settings.S3_ACCESS_KEY, settings.S3_SECRET_KEY = S3_KEY, S3_SECRET
    settings.MONGO_URL = mongo_url
    storage.reset_clients_for_tests()
    storage.replays().delete_many({})
    client = storage.s3()
    for bucket in ("round-logs", "drawings"):
        for obj in client.list_objects_v2(Bucket=bucket).get("Contents", []):
            client.delete_object(Bucket=bucket, Key=obj["Key"])
    yield
    storage.reset_clients_for_tests()


def stroke_log_bytes(
    match_id: str = "m_1",
    round_no: int = 1,
    strokes: list[dict[str, Any]] | None = None,
    width: int = 800,
    height: int = 600,
) -> bytes:
    """A gzipped doodle.game.v1.StrokeLog, as the game server uploads it."""
    import gzip

    from doodle.game.v1 import stroke_log_pb2

    log = stroke_log_pb2.StrokeLog(
        schema_version=1,
        match_id=match_id,
        room_id="r_1",
        round_no=round_no,
        canvas_width=width,
        canvas_height=height,
    )
    log.round_started_at.FromDatetime(datetime(2026, 10, 17, 12, 0, tzinfo=UTC))
    for i, s in enumerate(strokes if strokes is not None else [DEFAULT_STROKE]):
        stroke = log.strokes.add(
            stroke_id=f"s{i}",
            user_id=s.get("user_id", "u_ada"),
            color=s.get("color", "#1971c2"),
            width=s.get("width", 60),
            undone=s.get("undone", False),
            server_received_ms=100 * i,
        )
        for x, y, t in s["points"]:
            stroke.points.add(x=x, y=y, t_ms=t)
    return gzip.compress(log.SerializeToString())


DEFAULT_STROKE: dict[str, Any] = {"points": [(1000, 1000, 0), (5000, 5000, 40), (9000, 2000, 80)]}
