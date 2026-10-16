"""users and refresh_tokens

Revision ID: 0001
Revises:
"""

import sqlalchemy as sa
from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.create_table(
        "users",
        sa.Column("id", sa.Uuid, primary_key=True),
        sa.Column("google_sub", sa.Text, nullable=False),
        sa.Column("display_name", sa.Text, nullable=False),
        sa.Column("avatar_url", sa.Text),
        sa.Column("roles", sa.ARRAY(sa.Text), nullable=False, server_default=sa.text("'{player}'")),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
        sa.Column(
            "updated_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
        sa.Column("last_login_at", sa.DateTime(timezone=True)),
        sa.UniqueConstraint("google_sub", name="users_google_sub_key"),
        sa.CheckConstraint("cardinality(roles) > 0", name="users_roles_not_empty"),
    )
    op.create_table(
        "refresh_tokens",
        sa.Column("id", sa.Uuid, primary_key=True),
        sa.Column("family_id", sa.Uuid, nullable=False),
        sa.Column(
            "user_id", sa.Uuid, sa.ForeignKey("users.id", ondelete="CASCADE"), nullable=False
        ),
        sa.Column("token_hash", sa.LargeBinary, nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("used_at", sa.DateTime(timezone=True)),
        sa.Column("revoked_at", sa.DateTime(timezone=True)),
        sa.Column("revoked_reason", sa.Text),
        sa.UniqueConstraint("token_hash", name="refresh_tokens_token_hash_key"),
        sa.CheckConstraint("octet_length(token_hash) = 32", name="refresh_tokens_hash_is_sha256"),
    )
    op.create_index("refresh_tokens_family_id_idx", "refresh_tokens", ["family_id"])
    op.create_index("refresh_tokens_user_id_idx", "refresh_tokens", ["user_id"])


def downgrade() -> None:
    op.drop_table("refresh_tokens")
    op.drop_table("users")
