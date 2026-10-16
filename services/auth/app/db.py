"""Tables of the auth database. Alembic migrations in migrations/ create them; this module
describes them for queries (SQLAlchemy Core, so every statement stays explicit)."""

from sqlalchemy import (
    ARRAY,
    Column,
    DateTime,
    Engine,
    ForeignKey,
    Index,
    LargeBinary,
    MetaData,
    Table,
    Text,
    Uuid,
    create_engine,
    text,
)

metadata = MetaData()

users = Table(
    "users",
    metadata,
    Column("id", Uuid, primary_key=True),
    # Google's stable subject id. The only link to the Google account; no email is stored.
    Column("google_sub", Text, nullable=False, unique=True),
    Column("display_name", Text, nullable=False),
    Column("avatar_url", Text),
    Column("roles", ARRAY(Text), nullable=False, server_default=text("'{player}'")),
    Column("created_at", DateTime(timezone=True), nullable=False, server_default=text("now()")),
    Column("updated_at", DateTime(timezone=True), nullable=False, server_default=text("now()")),
    Column("last_login_at", DateTime(timezone=True)),
)

refresh_tokens = Table(
    "refresh_tokens",
    metadata,
    Column("id", Uuid, primary_key=True),
    # All tokens from one login share a family; reuse of any used token revokes the family.
    Column("family_id", Uuid, nullable=False),
    Column("user_id", Uuid, ForeignKey("users.id", ondelete="CASCADE"), nullable=False),
    # SHA-256 of the token. The token itself is never stored.
    Column("token_hash", LargeBinary, nullable=False, unique=True),
    Column("created_at", DateTime(timezone=True), nullable=False, server_default=text("now()")),
    Column("expires_at", DateTime(timezone=True), nullable=False),
    Column("used_at", DateTime(timezone=True)),
    Column("revoked_at", DateTime(timezone=True)),
    Column("revoked_reason", Text),
    Index("refresh_tokens_family_id_idx", "family_id"),
    Index("refresh_tokens_user_id_idx", "user_id"),
)


def make_engine(url: str) -> Engine:
    # pool_pre_ping survives Postgres restarts in the local stack.
    return create_engine(url, pool_pre_ping=True, pool_size=5, max_overflow=5)
