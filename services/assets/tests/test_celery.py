"""Celery against a real Redis broker: a worker takes a task, and the backlog gauge reads the age of
the oldest waiting message."""

import threading
import time
from collections.abc import Iterator
from typing import Any

import pytest
import redis
from celery import signals
from celery.contrib.testing.worker import start_worker
from django.conf import settings

from config.celery import app
from config.jobs import oldest_age_seconds
from drawings.tasks import ping


@pytest.fixture
def broker(redis_url: str) -> Iterator[redis.Redis]:
    # The app connects lazily, and only these tests use the broker, so pointing it at the
    # container (the same one for the whole session) before the first task is enough. The
    # namespaced key is the one config_from_object reads.
    app.conf.CELERY_BROKER_URL = redis_url
    client: redis.Redis = redis.Redis.from_url(redis_url)
    client.flushdb()
    yield client
    client.close()


def test_delivery_is_at_least_once() -> None:
    assert app.conf.task_acks_late
    assert app.conf.task_reject_on_worker_lost
    assert app.conf.worker_prefetch_multiplier == 1
    assert app.conf.task_ignore_result
    assert app.conf.task_default_queue == "assets"


def test_a_worker_runs_a_task_from_the_broker(broker: redis.Redis) -> None:
    done = threading.Event()
    results: list[Any] = []

    def on_success(sender: Any = None, result: Any = None, **kwargs: Any) -> None:
        results.append(result)
        done.set()

    signals.task_success.connect(on_success, sender=ping, weak=False)
    try:
        with start_worker(app, pool="solo", perform_ping_check=False, shutdown_timeout=10):
            ping.delay()
            assert done.wait(15), "the worker never ran the task"
    finally:
        signals.task_success.disconnect(on_success, sender=ping)
    assert results == ["pong"]
    assert broker.llen(settings.CELERY_KEY_PREFIX + "assets") == 0


def test_oldest_age_is_the_wait_of_the_oldest_message(broker: redis.Redis) -> None:
    queue = "age-test"
    # Every key Celery writes carries kombu's global prefix (the assets Redis user's).
    key = settings.CELERY_KEY_PREFIX + queue
    assert oldest_age_seconds(broker, key, time.time()) == 0.0

    sent = time.time()
    ping.apply_async(queue=queue)
    ping.apply_async(queue=queue)

    age = oldest_age_seconds(broker, key, sent + 30)
    assert 29 <= age <= 30
    assert broker.llen(key) == 2
    assert broker.llen(queue) == 0


def test_a_message_without_sent_at_counts_as_zero(broker: redis.Redis) -> None:
    broker.lpush("legacy", b'{"headers": {}}')

    assert oldest_age_seconds(broker, "legacy", time.time()) == 0.0


def test_the_broker_url_gets_the_redis_user() -> None:
    from config.settings import with_credentials

    assert with_credentials("redis://redis:6379/1", "", "") == "redis://redis:6379/1"
    assert (
        with_credentials("redis://redis:6379/1", "assets", "p@ss/word")
        == "redis://assets:p%40ss%2Fword@redis:6379/1"
    )
    # A URL that already names a user is replaced, not doubled.
    assert (
        with_credentials("redis://old:x@redis:6379/1", "assets", "s")
        == "redis://assets:s@redis:6379/1"
    )
