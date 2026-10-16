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
def client(db: Engine, key_file: str, google: FakeGoogle) -> Iterator[TestClient]:
    """The auth app on a real, empty database, with Google mocked."""
    settings = Settings(
        jwt_private_key_path=key_file,
        google_client_id=CLIENT_ID,
        google_client_secret=CLIENT_SECRET,
        oauth_state_secret="state-secret-" + "x" * 30,
    )
    ctx = build_context(settings, engine=db, http=httpx.Client())
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
        conn.execute(text("TRUNCATE refresh_tokens, users"))
    yield engine
