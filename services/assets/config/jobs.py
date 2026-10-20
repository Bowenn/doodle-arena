"""The queue backlog gauge, assets.jobs.oldest_age (docs/observability.md): how long the oldest
message waiting in a Celery queue has waited, in seconds, and 0 when the queue is empty.

The Redis transport keeps a queue as a list: producers push on the left and workers pop from the
right, so the oldest waiting message is the last element. Its `sent_at` header comes from
config/celery.py. Messages a worker has taken but not acknowledged yet are not waiting, so they
don't count; a stuck worker shows up as a growing age of the messages behind it.
"""

import json
import logging
import time
from collections.abc import Iterable

import redis
from opentelemetry import metrics
from opentelemetry.metrics import CallbackOptions, Observation

log = logging.getLogger(__name__)


def oldest_age_seconds(client: redis.Redis, key: str, now: float) -> float:
    """The wait of the oldest message in the queue stored at key (kombu's prefix included)."""
    raw = client.lindex(key, -1)
    if raw is None:
        return 0.0
    try:
        sent_at = json.loads(raw)["headers"]["sent_at"]
        return max(0.0, now - float(sent_at))
    except KeyError, TypeError, ValueError:
        return 0.0  # a message sent without the header (an older producer)


def register_queue_age_gauge(broker_url: str, queues: list[str], key_prefix: str = "") -> None:
    client: redis.Redis = redis.Redis.from_url(
        broker_url, socket_timeout=1, socket_connect_timeout=1
    )

    def observe(options: CallbackOptions) -> Iterable[Observation]:
        now = time.time()
        for queue in queues:
            try:
                age = oldest_age_seconds(client, key_prefix + queue, now)
            except redis.RedisError as e:
                log.warning(
                    "could not read the queue backlog", extra={"queue": queue, "error": str(e)}
                )
                continue
            yield Observation(age, {"queue": queue})

    metrics.get_meter(__name__).create_observable_gauge(
        "assets.jobs.oldest_age",
        callbacks=[observe],
        unit="s",
        description="Age of the oldest message waiting in a Celery queue",
    )
