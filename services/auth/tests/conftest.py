"""Shared fixtures. Integration tests run against a real Postgres started by Testcontainers
(never an in-memory substitute), migrated with the same Alembic scripts as production."""

import os
from collections.abc import Iterator
from pathlib import Path

import pytest
from alembic import command
from alembic.config import Config
from sqlalchemy import Engine, text

from app.db import make_engine

SERVICE_ROOT = Path(__file__).resolve().parent.parent

# Docker Desktop may not create /var/run/docker.sock; scripts/test.sh sets DOCKER_HOST from the
# current Docker context, and this fallback covers running pytest directly.
_desktop_socket = Path.home() / ".docker" / "run" / "docker.sock"
if "DOCKER_HOST" not in os.environ and _desktop_socket.exists():
    os.environ["DOCKER_HOST"] = f"unix://{_desktop_socket}"
    os.environ.setdefault("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock")

from testcontainers.community.postgres import PostgresContainer  # noqa: E402


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
