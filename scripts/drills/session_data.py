"""Data that must survive from one AWS session to the next (ADR 0016, the exit of task 6.10,
run in task 6.9).

    GATEWAY=https://icanfly.click DRILL_JWT_PEM=... uv run --project services/auth \\
        python scripts/drills/session_data.py record <file>     in session 1, after make loadtest
    GATEWAY=https://icanfly.click DRILL_JWT_PEM=... uv run --project services/auth \\
        python scripts/drills/session_data.py check <file>      in session 2, after aws-up restored

record works with two load-test players (Load0001 and Load0002, whose matches gave them coins
and drawings):
  - a webhook endpoint for the seller (notifier);
  - one of the seller's drawings listed and bought by the buyer, a sale (marketplace: coins,
    royalties, ownership);
  - then both wallets, both players' drawings with their owners, and the sold drawing's
    ownership history, written to <file> (ids and numbers only, no tokens).
check reads the same through the API of the restored stack and compares. Signing keys rotate
every session, so tokens are minted again with the new stack's key; the user ids are the same.
"""

import json
import os
import sys
import urllib.error
import urllib.request
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402

GATEWAY = os.environ.get("GATEWAY", "http://localhost:8080")
SELLER, BUYER = "u_load0001", "u_load0002"
ENDPOINT_URL = "https://example.com/doodle-arena-session-drill"


def tokens() -> dict[str, str]:
    key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                          os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"))
    issuer = TokenIssuer(key, GATEWAY + "/api/auth", timedelta(minutes=30))
    return {u: issuer.issue(u, ["player"], datetime.now(UTC), name=u)[0] for u in (SELLER, BUYER)}


def api(method: str, path: str, token: str, body: object = None, key: bool = False) -> tuple[int, dict]:
    req = urllib.request.Request(GATEWAY + path, method=method,
                                 data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Bearer " + token)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Idempotency-Key", str(uuid.uuid4()))
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, json.loads(raw) if raw else {}


def snapshot(tok: dict[str, str], sold: str) -> dict:
    """What the restore must bring back, as the API answers it."""
    out: dict = {"wallets": {}, "drawings": {}}
    for user in (SELLER, BUYER):
        out["wallets"][user] = api("GET", "/api/market/wallet", tok[user])[1]["balance"]
        owned = api("GET", "/api/market/drawings?owner=me&limit=50", tok[user])[1]["drawings"]
        out["drawings"][user] = sorted(d["drawing_id"] for d in owned)
    history = api("GET", f"/api/market/drawings/{sold}/ownership-history", tok[BUYER])[1]
    out["sold_history"] = {"owner_id": history["owner_id"], "events": [[e["kind"], e.get("to_owner")] for e in history["events"]]}
    code, image = api("GET", f"/api/assets/drawings/{sold}", tok[BUYER])
    out["sold_image"] = code == 200 and bool(image.get("image_url"))
    endpoints = api("GET", "/api/notifier/endpoints", tok[SELLER])[1].get("endpoints", [])
    out["endpoints"] = sorted(e["id"] for e in endpoints if e["url"] == ENDPOINT_URL)
    return out


def record(path: Path) -> int:
    tok = tokens()
    owned = api("GET", "/api/market/drawings?owner=me&limit=50", tok[SELLER])[1]["drawings"]
    unlisted = [d for d in owned if d.get("listing") is None]
    if not unlisted:
        sys.exit(f"{SELLER} owns no unlisted drawing: run make loadtest first")
    drawing = unlisted[0]["drawing_id"]
    code, endpoint = api("POST", "/api/notifier/endpoints", tok[SELLER],
                         {"url": ENDPOINT_URL, "event_types": ["sale.completed", "coins.credited"]})
    if code != 201:
        sys.exit(f"webhook endpoint: {code} {endpoint}")
    code, listing = api("POST", "/api/market/listings", tok[SELLER], {"drawing_id": drawing, "price": 5}, key=True)
    if code != 201:
        sys.exit(f"listing: {code} {listing}")
    code, sale = api("POST", f"/api/market/listings/{listing['listing_id']}/purchase", tok[BUYER], key=True)
    if code != 200:
        sys.exit(f"purchase: {code} {sale}")
    data = {"recorded_at": datetime.now(UTC).isoformat(), "sold_drawing": drawing, "listing_id": listing["listing_id"],
            "sale_id": sale["sale_id"], "endpoint_id": endpoint["id"], "expected": snapshot(tok, drawing)}
    path.write_text(json.dumps(data, indent=2) + "\n")
    print(json.dumps(data["expected"], indent=2))
    print(f"Recorded in {path}: run 'check' in the next session.")
    return 0


def check(path: Path) -> int:
    data = json.loads(path.read_text())
    tok = tokens()
    now = snapshot(tok, data["sold_drawing"])
    failed = 0
    for key, want in data["expected"].items():
        ok = now[key] == want
        failed += not ok
        print(f"  {'ok  ' if ok else 'FAIL'}  {key}" + ("" if ok else f": expected {want}, got {now[key]}"))
    code, listing = api("GET", f"/api/market/listings/{data['listing_id']}", tok[SELLER])
    ok = code == 200 and listing.get("status") == "SOLD" and listing.get("sale_id") == data["sale_id"]
    failed += not ok
    print(f"  {'ok  ' if ok else 'FAIL'}  the listing is SOLD by the recorded sale")
    print("session data: OK" if not failed else f"session data: {failed} FAILED")
    return 1 if failed else 0


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("record", "check"):
        sys.exit(__doc__)
    sys.exit(record(Path(sys.argv[2])) if sys.argv[1] == "record" else check(Path(sys.argv[2])))
