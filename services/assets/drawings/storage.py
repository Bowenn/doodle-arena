"""Object storage (SeaweedFS locally, S3 in AWS) and the MongoDB replays collection.

Clients are made lazily, once per process: Celery forks its pool processes, and neither boto3's
connection pool nor pymongo's monitor threads survive a fork. Every call has a timeout.
"""

import threading
import time
from collections import OrderedDict
from typing import Any

import boto3
import pymongo
from botocore.client import Config
from botocore.exceptions import BotoCoreError, ClientError
from django.conf import settings
from pymongo.collection import Collection
from pymongo.errors import PyMongoError

PNG = "image/png"
# Content-addressed objects never change, so clients and CDNs may cache them forever.
IMMUTABLE = "public, max-age=31536000, immutable"


class StrokeLogMissing(Exception):
    """The stroke log isn't in the bucket (it expired after 7 days, or was never uploaded)."""


# What a retry can fix: the store is down, slow or throttling. A ClientError other than a missing
# key (a 5xx, SlowDown, even AccessDenied after a bad deploy) is retried too: it's about the
# store, not the drawing, so it must never mark a drawing failed.
TRANSIENT_ERRORS: tuple[type[BaseException], ...] = (BotoCoreError, ClientError, PyMongoError)

# How long a presigned image URL works. Short, so a leaked URL soon stops working.
PRESIGN_SECONDS = 900
# The same URL is handed out again for this long, so a browser that already has the image finds it
# in its cache (a fresh signature is a new URL); every URL still has at least 5 minutes left.
PRESIGN_REUSE_SECONDS = 600
PRESIGN_CACHE_SIZE = 10_000

_lock = threading.Lock()
_s3: Any = None
_signer: Any = None
# key -> (url, when it was signed); the oldest entries go first once it's full.
_presigned: OrderedDict[str, tuple[str, float]] = OrderedDict()
_replays: Collection[dict[str, Any]] | None = None


def _client(endpoint: str) -> Any:
    """An S3 client for endpoint; an empty endpoint is AWS's own (boto3 refuses "")."""
    return boto3.client(
        "s3",
        endpoint_url=endpoint or None,
        region_name=settings.S3_REGION,
        aws_access_key_id=settings.S3_ACCESS_KEY or None,
        aws_secret_access_key=settings.S3_SECRET_KEY or None,
        config=Config(
            connect_timeout=3,
            read_timeout=10,
            retries={"max_attempts": 2, "mode": "standard"},
            s3={"addressing_style": "path" if settings.S3_PATH_STYLE else "virtual"},
            signature_version="s3v4",
        ),
    )


def s3() -> Any:
    global _s3
    with _lock:
        if _s3 is None:
            _s3 = _client(settings.S3_ENDPOINT)
        return _s3


def presigned_url(key: str) -> str:
    """A short-lived GET URL for a drawings object, signed for the address browsers use.
    Signing is local: no request to the store."""
    global _signer
    now = time.monotonic()
    with _lock:
        cached = _presigned.get(key)
        if cached and now - cached[1] < PRESIGN_REUSE_SECONDS:
            _presigned.move_to_end(key)
            return cached[0]
        if _signer is None:
            _signer = _client(settings.S3_PUBLIC_ENDPOINT)
        signer = _signer
    url: str = signer.generate_presigned_url(
        "get_object",
        Params={"Bucket": settings.DRAWINGS_BUCKET, "Key": key},
        ExpiresIn=PRESIGN_SECONDS,
    )
    with _lock:
        _presigned[key] = (url, now)
        _presigned.move_to_end(key)
        while len(_presigned) > PRESIGN_CACHE_SIZE:
            _presigned.popitem(last=False)
    return url


def replays() -> Collection[dict[str, Any]]:
    global _replays
    with _lock:
        if _replays is None:
            client: pymongo.MongoClient[dict[str, Any]] = pymongo.MongoClient(
                settings.MONGO_URL,
                serverSelectionTimeoutMS=3000,
                connectTimeoutMS=3000,
                socketTimeoutMS=10000,
                appname="assets",
            )
            _replays = client.get_default_database("assets")["replays"]
        return _replays


def reset_clients_for_tests() -> None:
    global _s3, _signer, _replays
    with _lock:
        _s3 = None
        _signer = None
        _replays = None
        _presigned.clear()


def _is_missing(e: ClientError) -> bool:
    return e.response.get("Error", {}).get("Code") in ("NoSuchKey", "404", "NotFound")


def get_stroke_log(key: str) -> bytes:
    try:
        resp = s3().get_object(Bucket=settings.ROUND_LOGS_BUCKET, Key=key)
        data: bytes = resp["Body"].read()
        return data
    except ClientError as e:
        if _is_missing(e):
            raise StrokeLogMissing(key) from e
        raise


def get_image(key: str) -> bytes:
    resp = s3().get_object(Bucket=settings.DRAWINGS_BUCKET, Key=key)
    data: bytes = resp["Body"].read()
    return data


def put_if_absent(key: str, data: bytes, content_type: str = PNG) -> bool:
    """Uploads a content-addressed object unless it's already there. Returns True if it uploaded.
    Two workers racing on the same key both upload the same bytes, which is harmless."""
    client = s3()
    try:
        client.head_object(Bucket=settings.DRAWINGS_BUCKET, Key=key)
        return False
    except ClientError as e:
        if not _is_missing(e):
            raise
    client.put_object(
        Bucket=settings.DRAWINGS_BUCKET,
        Key=key,
        Body=data,
        ContentType=content_type,
        CacheControl=IMMUTABLE,
    )
    return True


def save_replay(doc: dict[str, Any]) -> None:
    """Writes a replay once: a redelivered task finds it there and leaves it as it is."""
    replays().update_one({"_id": doc["_id"]}, {"$setOnInsert": doc}, upsert=True)


def get_replay(drawing_id: str) -> dict[str, Any] | None:
    return replays().find_one({"_id": drawing_id})
