"""Shared fixtures. Integration tests run against a real Postgres started by Testcontainers
(never an in-memory substitute), migrated with the same Alembic scripts as production."""

import os
from collections.abc import Iterator
from pathlib import Path

import httpx
import pytest
import respx
from alembic import command
from alembic.config import Config
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi.testclient import TestClient
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from sqlalchemy import Engine, text

from app.db import make_engine
from app.main import build_context, create_app
from app.settings import Settings
from tests.fake_google import CLIENT_ID, CLIENT_SECRET, FakeGoogle

SERVICE_ROOT = Path(__file__).resolve().parent.parent

# Docker Desktop may not create /var/run/docker.sock; scripts/test.sh sets DOCKER_HOST from the
# current Docker context, and this fallback covers running pytest directly.
_desktop_socket = Path.home() / ".docker" / "run" / "docker.sock"
if "DOCKER_HOST" not in os.environ and _desktop_socket.exists():
    os.environ["DOCKER_HOST"] = f"unix://{_desktop_socket}"
    os.environ.setdefault("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock")

from testcontainers.community.postgres import PostgresContainer  # noqa: E402


def new_rsa_pem() -> bytes:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    return key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )


@pytest.fixture(scope="session")
def key_file(tmp_path_factory: pytest.TempPathFactory) -> str:
    """A throwaway JWT signing key, like the one scripts/gen-jwt-key.sh creates."""
    path = tmp_path_factory.mktemp("keys") / "jwt-private.pem"
    path.write_bytes(new_rsa_pem())
    return str(path)


@pytest.fixture
def settings(key_file: str) -> Settings:
    return Settings(jwt_private_key_path=key_file, service_version="1.2.3")


@pytest.fixture
def google() -> Iterator[FakeGoogle]:
    """Google's endpoints, mocked. Unmatched outbound HTTP calls fail the test."""
    with respx.mock(assert_all_called=False) as router:
        yield FakeGoogle(router)


@pytest.fixture
def metric_reader() -> InMemoryMetricReader:
    """Collects the metrics the app records during one test. (Outside tests they go to the
    global meter provider, which is a no-op without a collector.)"""
    return InMemoryMetricReader()


@pytest.fixture
def client(
    db: Engine, key_file: str, google: FakeGoogle, metric_reader: InMemoryMetricReader
) -> Iterator[TestClient]:
    """The auth app on a real, empty database, with Google mocked."""
    settings = Settings(
        jwt_private_key_path=key_file,
        google_client_id=CLIENT_ID,
        google_client_secret=CLIENT_SECRET,
        oauth_state_secret="state-secret-" + "x" * 30,
    )
    meter = MeterProvider(metric_readers=[metric_reader]).get_meter("auth")
    ctx = build_context(settings, engine=db, http=httpx.Client(), meter=meter)
    with TestClient(create_app(settings, ctx), base_url="http://localhost:8080") as c:
        yield c


def alembic_config(engine: Engine) -> Config:
    cfg = Config(str(SERVICE_ROOT / "alembic.ini"))
    cfg.set_main_option("script_location", str(SERVICE_ROOT / "migrations"))
    cfg.attributes["engine"] = engine
    return cfg


def run_alembic(engine: Engine, action: str, revision: str) -> None:
    cfg = alembic_config(engine)
    with engine.begin() as conn:
        cfg.attributes["connection"] = conn
        getattr(command, action)(cfg, revision)


@pytest.fixture(scope="session")
def engine() -> Iterator[Engine]:
    with PostgresContainer("postgres:18.6-trixie", driver="psycopg") as pg:
        eng = make_engine(pg.get_connection_url())
        run_alembic(eng, "upgrade", "head")
        yield eng
        eng.dispose()


@pytest.fixture
def db(engine: Engine) -> Iterator[Engine]:
    """The migrated database, emptied before each test."""
    with engine.begin() as conn:
        conn.execute(text("TRUNCATE refresh_tokens, users, api_keys, outbox"))
    yield engine


KAFKA_IMAGE = "apache/kafka:4.2.2"


@pytest.fixture(scope="session")
def kafka_brokers() -> Iterator[str]:
    """A single KRaft broker, like the stack's, advertised on a fixed free host port (the same
    fixture as the assets tests)."""
    import socket
    import time

    from confluent_kafka.admin import AdminClient
    from confluent_kafka.cimpl import NewTopic
    from testcontainers.core.container import DockerContainer

    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
    env = {
        "KAFKA_NODE_ID": "1",
        "KAFKA_PROCESS_ROLES": "broker,controller",
        "KAFKA_CONTROLLER_QUORUM_VOTERS": "1@localhost:9093",
        "KAFKA_LISTENERS": "EXTERNAL://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093,INTERNAL://0.0.0.0:19092",
        "KAFKA_ADVERTISED_LISTENERS": f"EXTERNAL://localhost:{port},INTERNAL://localhost:19092",
        "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": (
            "EXTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT,INTERNAL:PLAINTEXT"
        ),
        "KAFKA_INTER_BROKER_LISTENER_NAME": "INTERNAL",
        "KAFKA_CONTROLLER_LISTENER_NAMES": "CONTROLLER",
        "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR": "1",
        "KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
        "KAFKA_TRANSACTION_STATE_LOG_MIN_ISR": "1",
        "KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS": "0",
        "KAFKA_AUTO_CREATE_TOPICS_ENABLE": "false",  # like the stack: topics are created on purpose
    }
    container = DockerContainer(KAFKA_IMAGE).with_bind_ports(9092, port)
    for k, v in env.items():
        container = container.with_env(k, v)
    with container:
        brokers = f"localhost:{port}"
        admin = AdminClient({"bootstrap.servers": brokers, "broker.address.family": "v4"})
        deadline = time.monotonic() + 90
        while True:
            try:
                admin.list_topics(timeout=2)
                break
            except Exception:
                if time.monotonic() > deadline:
                    raise
                time.sleep(0.5)
        for future in admin.create_topics([NewTopic("auth.users.v1", 1, 1)]).values():
            future.result(timeout=30)
        yield brokers
