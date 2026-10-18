"""api_keys

Revision ID: 0002
Revises: 0001
"""

import sqlalchemy as sa
from alembic import op

revision = "0002"
down_revision = "0001"
branch_labels = None
depends_on = None


def upgrade() -> None:
    op.create_table(
        "api_keys",
        sa.Column("id", sa.Uuid, primary_key=True),
        # The access token's subject. Text and no foreign key: drill tokens (scripts/drills)
        # have subjects without a users row, and a key only ever acts as its token's subject.
        sa.Column("user_id", sa.Text, nullable=False),
        sa.Column("name", sa.Text, nullable=False),
        # The public part of da_<prefix>_<secret>: finds the row without storing the secret.
        sa.Column("prefix", sa.Text, nullable=False),
        # SHA-256 of the secret part. The secret itself is never stored.
        sa.Column("secret_hash", sa.LargeBinary, nullable=False),
        sa.Column("scopes", sa.ARRAY(sa.Text), nullable=False),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
        sa.Column("last_used_at", sa.DateTime(timezone=True)),
        sa.Column("revoked_at", sa.DateTime(timezone=True)),
        sa.UniqueConstraint("prefix", name="api_keys_prefix_key"),
        sa.CheckConstraint("octet_length(secret_hash) = 32", name="api_keys_hash_is_sha256"),
        sa.CheckConstraint("cardinality(scopes) > 0", name="api_keys_scopes_not_empty"),
        sa.CheckConstraint("char_length(name) BETWEEN 1 AND 100", name="api_keys_name_length"),
    )
    op.create_index("api_keys_user_id_idx", "api_keys", ["user_id"])


def downgrade() -> None:
    op.drop_table("api_keys")
