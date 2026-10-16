import uuid

import pytest
from sqlalchemy import Engine, inspect, text
from sqlalchemy.exc import IntegrityError

from tests.conftest import run_alembic


def test_schema_has_both_tables(db: Engine) -> None:
    names = set(inspect(db).get_table_names())
    assert {"users", "refresh_tokens", "alembic_version"} <= names


def test_new_users_default_to_player_and_google_sub_is_unique(db: Engine) -> None:
    insert = text("INSERT INTO users (id, google_sub, display_name) VALUES (:id, :sub, 'A')")
    with db.begin() as conn:
        conn.execute(insert, {"id": uuid.uuid4(), "sub": "g-1"})
        roles = conn.execute(text("SELECT roles FROM users WHERE google_sub = 'g-1'")).scalar()
    assert roles == ["player"]

    with pytest.raises(IntegrityError), db.begin() as conn:
        conn.execute(insert, {"id": uuid.uuid4(), "sub": "g-1"})


def test_refresh_token_hash_must_be_sha256_sized(db: Engine) -> None:
    user_id = uuid.uuid4()
    with db.begin() as conn:
        conn.execute(
            text("INSERT INTO users (id, google_sub, display_name) VALUES (:id, 'g-2', 'B')"),
            {"id": user_id},
        )
    with pytest.raises(IntegrityError), db.begin() as conn:
        conn.execute(
            text(
                "INSERT INTO refresh_tokens (id, family_id, user_id, token_hash, expires_at) "
                "VALUES (:id, :fam, :uid, :hash, now())"
            ),
            {"id": uuid.uuid4(), "fam": uuid.uuid4(), "uid": user_id, "hash": b"short"},
        )


def test_downgrade_and_upgrade_round_trip(engine: Engine) -> None:
    run_alembic(engine, "downgrade", "base")
    assert "users" not in inspect(engine).get_table_names()
    run_alembic(engine, "upgrade", "head")
    assert "users" in inspect(engine).get_table_names()
