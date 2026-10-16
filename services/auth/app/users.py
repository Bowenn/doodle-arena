import uuid
from dataclasses import dataclass

from sqlalchemy import Connection, func, select
from sqlalchemy.dialects.postgresql import insert

from app.db import users
from app.oauth import GoogleIdentity


@dataclass(frozen=True)
class User:
    id: uuid.UUID
    display_name: str
    avatar_url: str | None
    roles: list[str]


def upsert_from_google(conn: Connection, identity: GoogleIdentity) -> User:
    """Creates the user on first login; afterwards refreshes their name and avatar.
    One row per Google account, guaranteed by the unique google_sub."""
    stmt = insert(users).values(
        id=uuid.uuid4(),
        google_sub=identity.subject,
        display_name=identity.name,
        avatar_url=identity.picture,
        last_login_at=func.now(),
    )
    upsert = stmt.on_conflict_do_update(
        index_elements=[users.c.google_sub],
        set_={
            "display_name": stmt.excluded.display_name,
            "avatar_url": stmt.excluded.avatar_url,
            "updated_at": func.now(),
            "last_login_at": func.now(),
        },
    ).returning(users.c.id, users.c.display_name, users.c.avatar_url, users.c.roles)
    row = conn.execute(upsert).one()
    return User(row.id, row.display_name, row.avatar_url, list(row.roles))


def get(conn: Connection, user_id: uuid.UUID) -> User | None:
    row = conn.execute(
        select(users.c.id, users.c.display_name, users.c.avatar_url, users.c.roles).where(
            users.c.id == user_id
        )
    ).one_or_none()
    return User(row.id, row.display_name, row.avatar_url, list(row.roles)) if row else None
