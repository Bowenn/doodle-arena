"""Refresh tokens: opaque, random, stored only as a SHA-256 hash, rotated on every use.

Every login starts a family. Using a token marks it used and issues its successor in the same
family. If a used (or revoked) token ever comes back, someone has a copy, so the whole family
is revoked and both the thief and the user have to sign in again (design doc 4.2).
"""

import hashlib
import secrets
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta

from sqlalchemy import Connection, insert, select, update

from app.db import refresh_tokens


def hash_token(token: str) -> bytes:
    return hashlib.sha256(token.encode()).digest()


def new_token() -> str:
    return secrets.token_urlsafe(32)


def _insert(
    conn: Connection, family_id: uuid.UUID, user_id: uuid.UUID, now: datetime, ttl: timedelta
) -> str:
    token = new_token()
    conn.execute(
        insert(refresh_tokens).values(
            id=uuid.uuid4(),
            family_id=family_id,
            user_id=user_id,
            token_hash=hash_token(token),
            created_at=now,
            expires_at=now + ttl,
        )
    )
    return token


def start_family(conn: Connection, user_id: uuid.UUID, now: datetime, ttl: timedelta) -> str:
    """Issues the first refresh token of a new login."""
    return _insert(conn, uuid.uuid4(), user_id, now, ttl)


@dataclass(frozen=True)
class RotateResult:
    """What happened to a presented refresh token. Returned rather than raised, so the caller's
    transaction commits in every case: a reuse-triggered family revocation must never be rolled
    back along with the rejected request."""

    outcome: str  # "rotated", "invalid", "expired" or "reuse_detected"
    token: str | None = None  # the successor, when rotated
    user_id: uuid.UUID | None = None
    family_id: uuid.UUID | None = None


def rotate(conn: Connection, token: str, now: datetime, ttl: timedelta) -> RotateResult:
    """Exchanges a refresh token for its successor. Must run inside a transaction: the row
    lock makes two concurrent uses of one token serialize, so exactly one of them wins and the
    others see a used token (reuse)."""
    row = conn.execute(
        select(refresh_tokens)
        .where(refresh_tokens.c.token_hash == hash_token(token))
        .with_for_update()
    ).one_or_none()
    if row is None:
        return RotateResult("invalid")
    if row.used_at is not None or row.revoked_at is not None:
        revoke_family(conn, row.family_id, now, "reuse_detected")
        return RotateResult("reuse_detected", user_id=row.user_id, family_id=row.family_id)
    if row.expires_at <= now:
        return RotateResult("expired", user_id=row.user_id, family_id=row.family_id)

    conn.execute(update(refresh_tokens).where(refresh_tokens.c.id == row.id).values(used_at=now))
    successor = _insert(conn, row.family_id, row.user_id, now, ttl)
    return RotateResult("rotated", successor, row.user_id, row.family_id)


def revoke_family(conn: Connection, family_id: uuid.UUID, now: datetime, reason: str) -> None:
    conn.execute(
        update(refresh_tokens)
        .where(refresh_tokens.c.family_id == family_id, refresh_tokens.c.revoked_at.is_(None))
        .values(revoked_at=now, revoked_reason=reason)
    )


def family_of(conn: Connection, token: str) -> uuid.UUID | None:
    return conn.execute(
        select(refresh_tokens.c.family_id).where(refresh_tokens.c.token_hash == hash_token(token))
    ).scalar_one_or_none()
