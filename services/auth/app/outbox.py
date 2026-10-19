"""Events auth publishes (ADR 0015), written to the outbox in the transaction that causes them.
The auth-relay process (app.relay) publishes them to Kafka afterwards, at least once."""

import uuid
from datetime import datetime

from doodle.events.v1 import meta_pb2, users_pb2
from google.protobuf.timestamp_pb2 import Timestamp
from opentelemetry import propagate
from sqlalchemy import Connection, insert

from app.db import outbox

USERS_TOPIC = "auth.users.v1"
PRODUCER = "auth"


def write(conn: Connection, topic: str, key: str, event_id: uuid.UUID, payload: bytes) -> None:
    carrier: dict[str, str] = {}
    propagate.inject(carrier)
    conn.execute(
        insert(outbox).values(
            event_id=event_id,
            topic=topic,
            key=key,
            payload=payload,
            traceparent=carrier.get("traceparent"),
        )
    )


def credentials_revoked(
    conn: Connection, user_id: str, revoked_at: datetime, reason: str = "refresh_token_reuse"
) -> uuid.UUID:
    """CredentialsRevoked on auth.users.v1: the notifier disables the user's webhook endpoints
    and refuses access tokens issued at or before revoked_at."""
    event_id = uuid.uuid4()
    at = Timestamp()
    at.FromDatetime(revoked_at)
    event = users_pb2.UserEvent(
        credentials_revoked=users_pb2.CredentialsRevoked(
            meta=meta_pb2.EventMeta(
                event_id=str(event_id),
                event_type="CredentialsRevoked",
                schema_version=1,
                occurred_at=at,
                producer=PRODUCER,
            ),
            user_id=user_id,
            reason=reason,
            revoked_at=at,
        )
    )
    write(conn, USERS_TOPIC, user_id, event_id, event.SerializeToString())
    return event_id
