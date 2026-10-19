"""Credential theft drill (task 4.R2, ADR 0015): a replayed refresh token makes auth revoke the
user's credentials, and the notifier disables their webhooks and refuses the old access token.

    uv run --project services/auth --with confluent-kafka --with 'protobuf>=7.36.2,<8' \\
        python scripts/drills/credential_theft.py

Against `make up` (with auth-relay and the webhook-receiver service; DRILL_JWT_KEY in a
worktree, like mint_tokens.py). What it does:

1. Creates a drill user and a refresh-token family inside the auth container, with auth's own
   code (the token travels on stdout into this process and is never written down).
2. With an access token issued now (the "thief's"), registers a coins.credited endpoint on the
   test receiver.
3. Rotates the refresh token once (the real user), then replays the old one (the thief): auth
   detects the theft. Times how long until the notifier shows the endpoint disabled.
4. Checks that the old access token gets 401 from the notifier and can't mint a new API key in
   auth, and that a match reward now reaches nobody.
5. With a token issued after the revocation, enables the endpoint (new secret) and checks that
   the next reward arrives, signed with the new secret.
"""

import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
sys.path.insert(0, str(ROOT / "proto" / "gen" / "python"))
sys.path.insert(0, str(Path(__file__).resolve().parent))
from app.tokens import SigningKey, TokenIssuer
from webhooks import RECEIVER, http, publish_match, wait

GW = "http://localhost:8080"
ORIGIN = {"Origin": GW}
COMPOSE = [
    "docker",
    "compose",
    "-f",
    str(ROOT / "infra" / "docker-compose.yml"),
    "-p",
    "doodle-arena",
    "--env-file",
    os.environ.get("ENV_FILE", str(ROOT / ".env")),
]

# Runs inside the auth container: a user row (refresh tokens reference users) and a new family.
IN_AUTH = """
import json, uuid
from datetime import UTC, datetime, timedelta
from app import refresh, users
from app.db import make_engine
from app.oauth import GoogleIdentity
from app.settings import Settings
engine = make_engine(Settings().database_url())
with engine.begin() as conn:
    user = users.upsert_from_google(conn, GoogleIdentity(subject="drill-theft-" + uuid.uuid4().hex, name="Theft drill", picture=None))
    token = refresh.start_family(conn, user.id, datetime.now(UTC), timedelta(days=1))
print(json.dumps({"user_id": str(user.id), "token": token}))
"""


def access_token(user: str) -> str:
    key = os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem")
    issuer = TokenIssuer(SigningKey.from_file(key), GW + "/api/auth", timedelta(minutes=30))
    return issuer.issue(user, ["player"], datetime.now(UTC), name="Theft drill")[0]


def refresh_with(token: str) -> tuple[int, str | None]:
    """POST /api/auth/refresh with this refresh cookie; returns the status and the new cookie."""
    req = urllib.request.Request(
        GW + "/api/auth/refresh",
        method="POST",
        headers={**ORIGIN, "Cookie": f"da_refresh={token}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            cookies = resp.headers.get_all("Set-Cookie") or []
            new = next(
                (c.split(";")[0].split("=", 1)[1] for c in cookies if c.startswith("da_refresh=")),
                None,
            )
            return resp.status, new
    except urllib.error.HTTPError as e:
        return e.code, None


def status(url: str, bearer: str) -> int:
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {bearer}"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        return e.code


def main() -> None:
    out = subprocess.run(
        [*COMPOSE, "exec", "-T", "auth", "python", "-c", IN_AUTH],
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    created = json.loads(out.strip().splitlines()[-1])
    user, first = created["user_id"], created["token"]
    print(f"user {user}, a refresh-token family started inside auth (the token is never printed)")

    stolen_access = access_token(user)
    thief = {"Authorization": f"Bearer {stolen_access}"}
    hook = "theft-" + uuid.uuid4().hex[:8]
    ep = http(
        "POST",
        f"{GW}/api/notifier/endpoints",
        {"url": f"http://webhook-receiver:9000/hooks/{hook}", "event_types": ["coins.credited"]},
        thief,
    )
    http("PUT", f"{RECEIVER}/admin/{hook}/secret", ep["secret"])
    print(f"endpoint {ep['id']} registered with the session's access token")

    code, second = refresh_with(first)
    assert code == 200 and second, f"rotation failed: {code}"
    print("the real user refreshed: the first token is now used")
    time.sleep(1.1)  # the revocation lands in a later second than any token issued so far
    code, _ = refresh_with(first)
    detected = time.monotonic()
    assert code == 401, f"replay answered {code}"
    print("the thief replayed the used token: 401, theft detected")

    time.sleep(1.1)  # a token whose iat is after the revocation
    fresh = {"Authorization": f"Bearer {access_token(user)}"}

    def disabled() -> Any:
        eps = http("GET", f"{GW}/api/notifier/endpoints", headers=fresh)["endpoints"]
        return next((e for e in eps if e["id"] == ep["id"] and e["status"] == "disabled"), None)

    found = wait("the endpoint to be disabled", 30, disabled)
    took = time.monotonic() - detected
    print(f"endpoint disabled {took:.1f} s after the replay (reason {found['disabled_reason']})")

    old = status(f"{GW}/api/notifier/endpoints", stolen_access)
    print(f"the thief's access token (still valid as a JWT) on the notifier: {old}")
    req = urllib.request.Request(
        f"{GW}/api/auth/api-keys",
        data=json.dumps({"name": "thief", "scopes": ["webhooks"]}).encode(),
        method="POST",
        headers={"Authorization": f"Bearer {stolen_access}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            mint = resp.status
    except urllib.error.HTTPError as e:
        mint = e.code
    print(f"the thief's access token minting a new API key in auth: {mint}")

    match = publish_match([user, "u_theft_other_" + uuid.uuid4().hex[:6]])
    time.sleep(5)
    got = http("GET", f"{RECEIVER}/admin/{hook}")["received"]
    deliveries = http("GET", f"{GW}/api/notifier/endpoints/{ep['id']}/deliveries", headers=fresh)
    print(
        f"reward {match} while disabled: receiver got {len(got)}, deliveries {len(deliveries['deliveries'])}"
    )

    enabled = http("POST", f"{GW}/api/notifier/endpoints/{ep['id']}/enable", {}, fresh)
    assert enabled["secret"] != ep["secret"]
    http("PUT", f"{RECEIVER}/admin/{hook}/secret", enabled["secret"])
    publish_match([user, "u_theft_other_" + uuid.uuid4().hex[:6]])
    after = wait(
        "a delivery after enabling", 60, lambda: http("GET", f"{RECEIVER}/admin/{hook}")["received"]
    )
    print(
        f"after enabling with a new secret: received {after[0]['event_type']}, verified: {after[0]['verified']}"
    )

    ok = old == 401 and mint == 401 and not got and not deliveries["deliveries"] and after[0]["verified"]
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
