"""The negative half of task 5.5's data ownership (run by scripts/data-acl-check.sh in a one-off
assets container, with every user's password in its environment): each service's Redis user and
S3 identity must be refused another service's keys and buckets, with a permission error, while
its own still work. The positive controls prove each credential is right, so a refusal is the
ACL's and not a failed login. It writes only under acl-check keys of the services' own prefixes.
"""

import os
import sys
import uuid
from collections.abc import Callable

import boto3
import redis
from botocore.client import Config
from botocore.exceptions import ClientError

failures = 0


def check(what: str, fn: Callable[[], object], refused: tuple[type[BaseException], ...] | None) -> None:
    """refused=None: fn must succeed. Otherwise fn must raise one of refused."""
    global failures
    try:
        fn()
    except BaseException as e:  # noqa: BLE001 - the check reports any outcome
        if refused and isinstance(e, refused) and not _is_s3_error(e, "NoSuchKey", "404"):
            print(f"  ok    {what}")
            return
        print(f"  FAIL  {what}: {type(e).__name__}: {str(e)[:120]}")
        failures += 1
        return
    if refused is None:
        print(f"  ok    {what}")
    else:
        print(f"  FAIL  {what}: allowed")
        failures += 1


def _is_s3_error(e: BaseException, *codes: str) -> bool:
    return isinstance(e, ClientError) and e.response.get("Error", {}).get("Code") in codes


class S3Denied(Exception):
    pass


def s3_call(fn: Callable[[], object]) -> Callable[[], object]:
    """Turns an AccessDenied into S3Denied, so check() can tell it from a missing object."""

    def run() -> object:
        try:
            return fn()
        except ClientError as e:
            if _is_s3_error(e, "AccessDenied", "403", "InvalidAccessKeyId", "SignatureDoesNotMatch"):
                raise S3Denied(e.response["Error"]["Code"]) from e
            raise

    return run


def rds(user: str | None, password: str | None) -> redis.Redis:
    return redis.Redis(host="redis", port=6379, username=user, password=password, socket_timeout=3)


def s3(access: str | None, secret: str | None) -> object:
    cfg = Config(s3={"addressing_style": "path"}, signature_version="s3v4", retries={"max_attempts": 1})
    if access is None:
        from botocore import UNSIGNED

        cfg = Config(signature_version=UNSIGNED, s3={"addressing_style": "path"}, retries={"max_attempts": 1})
    return boto3.client(
        "s3", endpoint_url="http://seaweedfs:8333", region_name="us-east-1",
        aws_access_key_id=access, aws_secret_access_key=secret, config=cfg,
    )


def main() -> int:
    env = os.environ
    lobby = rds("lobby", env["REDIS_LOBBY_PASSWORD"])
    game = rds("game-server", env["REDIS_GAME_SERVER_PASSWORD"])
    assets = rds("assets", env["REDIS_ASSETS_PASSWORD"])
    perm = (redis.exceptions.NoPermissionError,)
    auth = (redis.exceptions.AuthenticationError, redis.exceptions.ResponseError)
    probe = f"acl-check-{uuid.uuid4().hex[:8]}"

    print("Redis users (one per key prefix):")
    check("a client with no credential is refused", lambda: rds(None, None).get("lobby:names"), auth)
    check("a wrong password is refused", lambda: rds("lobby", "not-the-password").ping(), auth)
    check("lobby works on lobby:* (control)", lambda: lobby.set(f"lobby:{probe}", "1", ex=60), None)
    check("game-server works on room:* (control)", lambda: game.set(f"room:{{{probe}}}:x", "1", ex=60), None)
    check("assets works on assets:* (control)", lambda: assets.set(f"assets:{probe}", "1", ex=60), None)
    check("lobby can't read a room (room:*)", lambda: lobby.get(f"room:{{{probe}}}:x"), perm)
    check("game-server can't write lobby:names", lambda: game.hset("lobby:names", "u_x", "x"), perm)
    check("assets can't read lobby:names", lambda: assets.hgetall("lobby:names"), perm)
    check("assets can't redeem a join ticket (room:{id}:ticket:*)",
          lambda: assets.set(f"room:{{{probe}}}:ticket:x", "1", nx=True, ex=60), perm)
    check("game-server can't queue a Celery task (assets:*)", lambda: game.lpush("assets:celery", "x"), perm)
    check("lobby can't queue a Celery task (assets:*)", lambda: lobby.lpush("assets:celery", "x"), perm)
    check("game-server can't FLUSHALL", lambda: game.flushall(), perm)
    check("lobby can't list every key (KEYS)", lambda: lobby.keys("*"), perm)
    check("lobby can't list key names with SCAN", lambda: lobby.scan(0), perm)
    check("assets can't list key names with RANDOMKEY", lambda: assets.randomkey(), perm)

    print("S3 identities (one per service, scoped to its buckets):")
    gs3 = s3("game-server", env["S3_GAME_SERVER_SECRET_KEY"])
    as3 = s3("assets", env["S3_ASSETS_SECRET_KEY"])
    ans3 = s3("analytics", env["S3_ANALYTICS_SECRET_KEY"])
    denied = (S3Denied,)
    check("game-server writes round-logs (control)",
          s3_call(lambda: gs3.put_object(Bucket="round-logs", Key=f"acl-check/{probe}", Body=b"x")), None)
    check("assets reads round-logs (control)",
          s3_call(lambda: as3.get_object(Bucket="round-logs", Key=f"acl-check/{probe}")), None)
    check("anonymous can't read round-logs",
          s3_call(lambda: s3(None, None).get_object(Bucket="round-logs", Key=f"acl-check/{probe}")), denied)
    check("game-server can't read drawings",
          s3_call(lambda: gs3.get_object(Bucket="drawings", Key="acl-check/none")), denied)
    check("game-server can't write drawings",
          s3_call(lambda: gs3.put_object(Bucket="drawings", Key=f"acl-check/{probe}", Body=b"x")), denied)
    check("assets can't write round-logs (forge a stroke log)",
          s3_call(lambda: as3.put_object(Bucket="round-logs", Key=f"acl-check/{probe}", Body=b"x")), denied)
    check("assets can't delete a stroke log",
          s3_call(lambda: as3.delete_object(Bucket="round-logs", Key=f"acl-check/{probe}")), denied)
    check("analytics writes analytics-checkpoints (control)",
          s3_call(lambda: ans3.put_object(Bucket="analytics-checkpoints", Key=f"acl-check/{probe}", Body=b"x")), None)
    check("analytics can't read round-logs",
          s3_call(lambda: ans3.get_object(Bucket="round-logs", Key=f"acl-check/{probe}")), denied)
    check("analytics can't read drawings",
          s3_call(lambda: ans3.get_object(Bucket="drawings", Key="acl-check/none")), denied)
    check("assets can't read analytics' checkpoints",
          s3_call(lambda: as3.get_object(Bucket="analytics-checkpoints", Key=f"acl-check/{probe}")), denied)
    check("analytics deletes its probe (control)",
          s3_call(lambda: ans3.delete_object(Bucket="analytics-checkpoints", Key=f"acl-check/{probe}")), None)

    if failures:
        print(f"data-acl-check: {failures} check(s) failed")
        return 1
    print("data-acl-check: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
