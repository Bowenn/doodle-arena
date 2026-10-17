"""Mints access tokens for drill players with auth's own TokenIssuer and the local development
key, so live tests need no Google sign-in. Prints JSON: [{"id", "name", "token"}, ...].

    uv run --project services/auth python scripts/drills/mint_tokens.py Ada Bob Cyd

Each name becomes the user u_<name in lower case>. The key is secrets/jwt-private.pem, or
DRILL_JWT_KEY when set (a worktree has no secrets/, so point it at the main checkout's key
instead of copying it). The tokens last 90 minutes; never write them to a file.
"""

import json
import os
import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402

GW = "http://localhost:8080"


def main(names):
    key = os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem")
    issuer = TokenIssuer(SigningKey.from_file(key), GW + "/api/auth", timedelta(minutes=90))
    now = datetime.now(UTC)
    players = []
    for name in names:
        user = "u_" + name.lower()
        token = issuer.issue(user, ["player"], now, name=name)[0]
        players.append({"id": user, "name": name, "token": token})
    print(json.dumps(players))


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("usage: mint_tokens.py <name> [<name> ...]")
    main(sys.argv[1:])
