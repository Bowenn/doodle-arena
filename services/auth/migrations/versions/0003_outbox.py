"""outbox

Revision ID: 0003
Revises: 0002
"""

import sqlalchemy as sa
from alembic import op

revision = "0003"
down_revision = "0002"
branch_labels = None
depends_on = None


def upgrade() -> None:
    # The transactional outbox (ADR 0015): an event is written in the transaction that causes it,
    # and the auth-relay process publishes it to Kafka afterwards.
    op.create_table(
        "outbox",
        # Also the event's meta.event_id, so consumers dedupe on it.
        sa.Column("event_id", sa.Uuid, primary_key=True),
        sa.Column("topic", sa.Text, nullable=False),
        sa.Column("key", sa.Text, nullable=False),
        sa.Column("payload", sa.LargeBinary, nullable=False),
        # The trace context the event was written under, so its publish joins that trace.
        sa.Column("traceparent", sa.Text),
        sa.Column(
            "created_at", sa.DateTime(timezone=True), nullable=False, server_default=sa.func.now()
        ),
        sa.Column("published_at", sa.DateTime(timezone=True)),
    )
    op.create_index(
        "outbox_unpublished_idx",
        "outbox",
        ["created_at"],
        postgresql_where=sa.text("published_at IS NULL"),
    )


def downgrade() -> None:
    op.drop_table("outbox")
