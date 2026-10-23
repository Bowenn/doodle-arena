"""Mints access tokens for drill players with auth's own TokenIssuer and the local development
key, so live tests need no Google sign-in. Prints JSON: [{"id", "name", "token"}, ...].

    uv run --project services/auth python scripts/drills/mint_tokens.py Ada Bob Cyd

Each name becomes the user u_<name in lower case>, with the player role. --roles gives every
minted user other roles instead, for example a moderator for the Django admin:

    uv run --project services/auth python scripts/drills/mint_tokens.py --roles player,moderator Mo

--ids takes user ids as they are instead, for users that already exist (the tips load test uses
make seed's seed-u-<n>):

    uv run --project services/auth python scripts/drills/mint_tokens.py --ids seed-u-1 seed-u-2

The key is secrets/jwt-private.pem, or DRILL_JWT_KEY when set (a worktree has no secrets/, so
point it at the main checkout's key instead of copying it). The tokens last 90 minutes; never write them to a file.

Against AWS (ADR 0003), the key is the stack's own, read from SSM into DRILL_JWT_PEM (never into a
file), and GATEWAY names the issuer's origin:

    DRILL_JWT_PEM="$(aws ssm get-parameter --with-decryption \
        --name /doodle-arena/stack/jwt-private-key --query Parameter.Value --output text)" \
        GATEWAY=https://icanfly.click uv run ... mint_tokens.py Ada
"""

import json
import os
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402

GW = os.environ.get("GATEWAY", "http://localhost:8080")


def main(names, roles, ids=False):
    key = os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem")
    signing_key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""), key)
    issuer = TokenIssuer(signing_key, GW + "/api/auth", timedelta(minutes=90))
    now = datetime.now(UTC)
    players = []
    for name in names:
        user = name if ids else "u_" + name.lower()
        token = issuer.issue(user, roles, now, name=name)[0]
        players.append({"id": user, "name": name, "token": token})
    print(json.dumps(players))


if __name__ == "__main__":
    args = sys.argv[1:]
    roles = ["player"]
    ids = bool(args) and args[0] == "--ids"
    if ids:
        args = args[1:]
    if len(args) >= 2 and args[0] == "--roles":
        roles = [r for r in args[1].split(",") if r]
        args = args[2:]
    if not args or not roles:
        sys.exit("usage: mint_tokens.py [--ids | --roles <role,role>] <name> [<name> ...]")
    main(args, roles, ids)
