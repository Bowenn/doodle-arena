"""API keys for developers (design doc 4.2): `da_<prefix>_<secret>`.

The prefix (12 random characters) finds the row; the secret (32 random bytes) is stored only as
its SHA-256. A plain hash is enough here, unlike for passwords: the secret is random and long, so
there is nothing to guess. A key is shown once, when it is created.
"""

import hashlib
import secrets
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import Literal

from sqlalchemy import Connection, func, insert, select, update

from app.db import api_keys

PREFIX_LENGTH = 12
MAX_ACTIVE_KEYS = 10
SCOPES = frozenset({"webhooks"})
# last_used_at is a hint for the owner, not an audit log: update it at most once a minute.
LAST_USED_GRANULARITY = timedelta(minutes=1)
_PREFIX_ALPHABET = "abcdefghijkmnpqrstuvwxyz23456789"  # no 0/o or 1/l


@dataclass(frozen=True)
class ApiKey:
    id: uuid.UUID
    user_id: str
    name: str
    prefix: str
    scopes: list[str]
    created_at: datetime
    last_used_at: datetime | None


@dataclass(frozen=True)
class Verified:
    key_id: uuid.UUID
    user_id: str
    scopes: list[str]


VerifyResult = Literal["valid", "invalid", "revoked"]


def hash_secret(secret: str) -> bytes:
    return hashlib.sha256(secret.encode()).digest()


def new_key() -> tuple[str, str, str]:
    """(full key, prefix, secret)."""
    prefix = "".join(secrets.choice(_PREFIX_ALPHABET) for _ in range(PREFIX_LENGTH))
    secret = secrets.token_urlsafe(32)
    return f"da_{prefix}_{secret}", prefix, secret


def parse(key: str) -> tuple[str, str] | None:
    """(prefix, secret), or None when the key isn't shaped like one of ours."""
    parts = key.split("_", 2)
    if len(parts) != 3 or parts[0] != "da" or len(parts[1]) != PREFIX_LENGTH or not parts[2]:
        return None
    return parts[1], parts[2]


def _row(row) -> ApiKey:  # type: ignore[no-untyped-def]
    return ApiKey(
        id=row.id,
        user_id=row.user_id,
        name=row.name,
        prefix=row.prefix,
        scopes=list(row.scopes),
        created_at=row.created_at,
        last_used_at=row.last_used_at,
    )


def active_count(conn: Connection, user_id: str) -> int:
    stmt = select(func.count()).where(
        api_keys.c.user_id == user_id, api_keys.c.revoked_at.is_(None)
    )
    return int(conn.execute(stmt).scalar_one())


def create(conn: Connection, user_id: str, name: str, scopes: list[str]) -> tuple[ApiKey, str]:
    """Stores a new key and returns it with the full key text, which is never stored."""
    key, prefix, secret = new_key()
    row = conn.execute(
        insert(api_keys)
        .values(
            id=uuid.uuid4(),
            user_id=user_id,
            name=name,
            prefix=prefix,
            secret_hash=hash_secret(secret),
            scopes=sorted(set(scopes)),
        )
        .returning(api_keys)
    ).one()
    return _row(row), key


def list_active(conn: Connection, user_id: str) -> list[ApiKey]:
    stmt = (
        select(api_keys)
        .where(api_keys.c.user_id == user_id, api_keys.c.revoked_at.is_(None))
        .order_by(api_keys.c.created_at.desc())
    )
    return [_row(r) for r in conn.execute(stmt)]


def revoke(conn: Connection, user_id: str, key_id: uuid.UUID, now: datetime) -> bool:
    """Revokes the caller's own key; False when there is no such active key of theirs."""
    stmt = (
        update(api_keys)
        .where(
            api_keys.c.id == key_id,
            api_keys.c.user_id == user_id,
            api_keys.c.revoked_at.is_(None),
        )
        .values(revoked_at=now)
    )
    return conn.execute(stmt).rowcount == 1


def verify(conn: Connection, key: str, now: datetime) -> tuple[VerifyResult, Verified | None]:
    parsed = parse(key)
    if parsed is None:
        return "invalid", None
    prefix, secret = parsed
    row = conn.execute(select(api_keys).where(api_keys.c.prefix == prefix)).one_or_none()
    # Constant time: how much of a wrong secret matched must not show in the response time.
    if row is None or not secrets.compare_digest(row.secret_hash, hash_secret(secret)):
        return "invalid", None
    if row.revoked_at is not None:
        return "revoked", None
    if row.last_used_at is None or now - row.last_used_at >= LAST_USED_GRANULARITY:
        conn.execute(update(api_keys).where(api_keys.c.id == row.id).values(last_used_at=now))
    return "valid", Verified(key_id=row.id, user_id=row.user_id, scopes=list(row.scopes))
