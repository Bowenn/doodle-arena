"""End-to-end check of the drawing pipeline on `make up` (run by scripts/pipeline-check.sh in a
one-off assets container, which has Kafka as dev-tools, S3 and the contracts): uploads a stroke log, publishes one
RoundFinished like the game server does, and waits for the drawing to be approved, its image to be
in the drawings bucket and its DrawingApproved to be published. Exits 1 with the last state if it
doesn't get there. The stroke is random, so the image is new and is really uploaded.
"""

import gzip
import os
import random
import sys
import time
import uuid
from datetime import UTC, datetime

import django

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")
django.setup()

from confluent_kafka import Producer  # noqa: E402
from django.conf import settings  # noqa: E402
from doodle.events.v1 import rounds_pb2  # noqa: E402
from doodle.game.v1 import stroke_log_pb2  # noqa: E402

from drawings import storage  # noqa: E402
from drawings.kafka_auth import kafka_auth  # noqa: E402
from drawings.models import Drawing, OutboxEvent  # noqa: E402

TIMEOUT_S = 90


def main() -> int:
    match_id = f"m_check_{uuid.uuid4().hex[:12]}"
    key = f"{match_id}/1.binpb.gz"
    started = time.monotonic()

    log = stroke_log_pb2.StrokeLog(
        schema_version=1, match_id=match_id, room_id="r_check", round_no=1, canvas_width=800, canvas_height=600
    )
    log.round_started_at.FromDatetime(datetime.now(UTC))
    stroke = log.strokes.add(stroke_id="s1", user_id="u_check", color="#1971c2", width=60, server_received_ms=500)
    for i in range(8):
        stroke.points.add(x=random.randint(500, 9500), y=random.randint(500, 9500), t_ms=i * 50)
    storage.s3().put_object(Bucket=settings.ROUND_LOGS_BUCKET, Key=key, Body=gzip.compress(log.SerializeToString()))

    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id = str(uuid.uuid4())
    rf.meta.event_type, rf.meta.schema_version, rf.meta.producer = "RoundFinished", 1, "pipeline-check"
    rf.meta.occurred_at.GetCurrentTime()
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, "r_check", 1, "check"
    rf.drawer_ids.append("u_check")
    rf.stroke_log_key, rf.stroke_count, rf.canvas_width, rf.canvas_height = key, 1, 800, 600
    rf.ended_at.GetCurrentTime()
    producer = Producer({"bootstrap.servers": settings.KAFKA_BROKERS, **kafka_auth()})
    producer.produce("game.rounds.v1", key=b"r_check", value=event.SerializeToString())
    if producer.flush(10):
        print("FAIL: RoundFinished not acknowledged by Kafka")
        return 1
    print(f"published RoundFinished for {match_id}")

    state = "no drawing yet"
    while time.monotonic() - started < TIMEOUT_S:
        drawing = Drawing.objects.filter(match_id=match_id).first()
        if drawing is not None:
            event_row = OutboxEvent.objects.filter(key=str(drawing.id)).first()
            state = (
                f"status={drawing.status} image={'yes' if drawing.image_key else 'no'} "
                f"event={'published' if event_row and event_row.published_at else 'not yet'} "
                f"reason={drawing.decision_reason!r}"
            )
            if drawing.status == "approved" and event_row is not None and event_row.published_at:
                storage.s3().head_object(Bucket=settings.DRAWINGS_BUCKET, Key=drawing.image_key)
                print(f"ok: {drawing.id} approved, image stored, DrawingApproved published "
                      f"{time.monotonic() - started:.1f} s after the round")
                return 0
            if drawing.status in ("rejected", "failed", "review"):
                break
        time.sleep(0.5)
    print(f"FAIL after {time.monotonic() - started:.0f} s: {state}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
