"""Failover drill (task 2.5, ADR 0007): three bots play a round through the gateway, then the
game server that owns the room is killed (or paused, to make a zombie). The drill measures how
long until every bot has resumed on the other server, and checks that no bot missed or saw a
duplicate event.

Run it from the repo root against a running stack (make up). Access tokens are signed with the
local development key in secrets/, through auth's own TokenIssuer:

    uv run --project services/auth --with websockets python scripts/drills/failover.py kill
    uv run --project services/auth --with websockets python scripts/drills/failover.py pause
    uv run --project services/auth --with websockets python scripts/drills/failover.py kill 5

An optional player count (default 3) plays the round with more bots: from 4, two of them share
the canvas, and the drill also checks that both drawers get their word back after the move.

kill:  docker kill on the owner. pause: docker pause for 9 s (the lobby reassigns the room
after 6 s without heartbeats), then unpause: the zombie's next lease renewal or write fails
the fencing check, and its log says so.

stop:  in AWS (task 5.3, ADR 0003), `aws ecs stop-task` on the owner's task: SIGTERM, as a deploy
or a scale-in sends it (a crash like docker kill isn't possible on Fargate: the image has no
shell). ECS starts a replacement. Set GATEWAY to the stack's URL; the tokens are signed with the
stack's key, read from SSM into DRILL_JWT_PEM (mint_tokens.py shows how):

    GATEWAY=https://icanfly.click DRILL_JWT_PEM="$(aws ssm get-parameter ...)" \
        uv run --project services/auth --with websockets python scripts/drills/failover.py stop
"""

import asyncio
import contextlib
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import UTC, datetime, timedelta
from pathlib import Path

import websockets

# PIN_HOST and PIN_IP skip the local resolver, which may still cache a "no such name" from
# before a fresh AWS stack's record existed; TLS still checks the certificate for PIN_HOST.
if os.environ.get("PIN_HOST"):
    import socket

    _getaddrinfo = socket.getaddrinfo

    def _pinned(host, *args, **kwargs):
        if host == os.environ["PIN_HOST"]:
            host = os.environ["PIN_IP"]
        return _getaddrinfo(host, *args, **kwargs)

    socket.getaddrinfo = _pinned

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402

GW = os.environ.get("GATEWAY", "http://localhost:8080")
WS, ORIGIN = GW.replace("http", "ws", 1), GW
issuer = TokenIssuer(
    SigningKey.load(
        os.environ.get("DRILL_JWT_PEM", ""),
        os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"),
    ),
    GW + "/api/auth",
    timedelta(minutes=10),
)


def call(method, path, user, body=None):
    req = urllib.request.Request(GW + path, method=method, data=json.dumps(body or {}).encode())
    token = issuer.issue(user, ["player"], datetime.now(UTC), name=user)[0]
    req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read())


class Bot:
    """A player that reconnects through the lobby and resumes from its last seq, like the web."""

    def __init__(self, user, room_id):
        self.user, self.room_id = user, room_id
        self.seqs, self.private, self.server = [], [], None
        self.ws = None
        self.drops = 0
        self.welcomes = 0
        self.resumed_at = None  # first message from a new server after a drop
        self.dropped_at = None

    @property
    def last_seq(self):
        return self.seqs[-1] if self.seqs else 0

    async def connect(self):
        while True:
            try:
                j = call("POST", f"/api/lobby/rooms/{self.room_id}/join", self.user)
                self.ws = await websockets.connect(WS + j["ws_url"], origin=ORIGIN)
                await self.ws.send(
                    json.dumps(
                        {
                            "type": "hello",
                            "data": {"ticket": j["ticket"], "last_seq": self.last_seq},
                        }
                    )
                )
                self.server = j["ws_url"].split("/")[2]
                return
            except urllib.error.HTTPError, OSError, websockets.WebSocketException:
                await asyncio.sleep(0.25)  # the lobby says 503 while the room moves

    async def send(self, typ, data):
        with contextlib.suppress(websockets.WebSocketException):
            await self.ws.send(json.dumps({"type": typ, "data": data}))

    async def run(self, stop):
        await self.connect()
        while not stop.is_set():
            try:
                raw = await asyncio.wait_for(self.ws.recv(), 1)
            except TimeoutError:
                continue
            except websockets.ConnectionClosed:
                self.drops += 1
                self.dropped_at = self.dropped_at or time.monotonic()
                await self.connect()
                continue
            m = json.loads(raw)
            if self.dropped_at and not self.resumed_at:
                self.resumed_at = time.monotonic()
            if m["type"] == "welcome":
                # A full view replaces the state: the seq order starts again from here.
                if self.dropped_at:
                    self.welcomes += 1  # a full view instead of a resume after the drop
                self.seqs = [m["data"]["seq"]]
            elif m["type"] == "event":
                self.seqs.append(m["data"]["seq"])
            else:
                self.private.append(m)

    def gaps(self):
        """Seqs out of order or repeated (a welcome resets the base)."""
        bad = []
        for a, b in zip(self.seqs, self.seqs[1:], strict=False):
            if b != a + 1:
                bad.append((a, b))
        return bad


def stop_ecs_task(service):
    """Stops the running task of an ECS service (a game server's server id is its service)."""
    profile = os.environ.get("AWS_PROFILE", "doodle-arena")
    aws = ["aws", "--profile", profile, "--region", "us-west-2"]
    task = subprocess.run(
        [
            *aws,
            "ecs",
            "list-tasks",
            "--cluster",
            "doodle-arena",
            "--service-name",
            service,
            "--query",
            "taskArns[0]",
            "--output",
            "text",
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    subprocess.run(
        [
            *aws,
            "ecs",
            "stop-task",
            "--cluster",
            "doodle-arena",
            "--task",
            task,
            "--reason",
            "failover drill (scripts/drills/failover.py stop)",
        ],
        check=True,
        capture_output=True,
    )


async def main(mode, players):
    room = call(
        "POST",
        "/api/lobby/rooms",
        "drill-host",
        {"settings": {"draw_seconds": 120, "choose_seconds": 30}},
    )
    rid = room["room_id"]
    users = ["drill-host"] + [f"drill-{chr(ord('b') + i)}" for i in range(players - 1)]
    bots = [Bot(u, rid) for u in users]
    stop = asyncio.Event()
    # The host joins first: whoever joins before the lobby's host becomes host (design doc, 7.3).
    tasks = [asyncio.create_task(bots[0].run(stop))]
    while bots[0].last_seq == 0:
        await asyncio.sleep(0.05)
    tasks += [asyncio.create_task(b.run(stop)) for b in bots[1:]]
    await asyncio.sleep(1)
    host = bots[0]
    await host.send("start", {})
    drawer = None
    for _ in range(100):  # the 3 s countdown, then word options for the drawer
        drawer = next(
            (b for b in bots if any(p["type"] == "word_options" for p in b.private)), None
        )
        if drawer:
            break
        await asyncio.sleep(0.1)
    if drawer is None:
        sys.exit("no drawer got word options: is the stack healthy?")
    await drawer.send("choose_word", {"index": 0})
    await asyncio.sleep(0.5)
    drawers = [b for b in bots if any(p["type"] == "your_word" for p in b.private)]
    print(f"{len(drawers)} drawer(s): {', '.join(b.user for b in drawers)}")
    for i in range(10):
        await drawer.send(
            "stroke_batch",
            {
                "stroke_id": f"s{i}",
                "points": [[100 * i, 100, i]],
                "color": "#112233",
                "width": 3,
                "done": True,
            },
        )
        await asyncio.sleep(0.1)
    owner = bots[0].server
    print(f"room {rid} on {owner}; seq {bots[0].last_seq}; {mode} {owner} now")

    t0 = time.monotonic()
    if mode == "kill":
        subprocess.run(
            ["docker", "kill", f"doodle-arena-{owner}-1"], check=True, capture_output=True
        )
    elif mode == "stop":
        stop_ecs_task(owner)
    else:
        subprocess.run(
            ["docker", "pause", f"doodle-arena-{owner}-1"], check=True, capture_output=True
        )

        async def unpause_later():
            # Long enough for the lobby to see no heartbeats (6 s) and move the room.
            await asyncio.sleep(9)
            subprocess.run(
                ["docker", "unpause", f"doodle-arena-{owner}-1"], check=True, capture_output=True
            )
            print(f"unpaused {owner} after 9 s")

        unpause = asyncio.create_task(unpause_later())

    # The drawer keeps drawing during the outage, as a real player would.
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline and not all(b.resumed_at for b in bots):
        await drawer.send(
            "stroke_batch",
            {
                "stroke_id": "late",
                "points": [[5, 5, 1]],
                "color": "#112233",
                "width": 3,
                "done": False,
            },
        )
        await asyncio.sleep(0.5)
    if mode == "pause":
        await unpause
        await asyncio.sleep(4)  # the zombie's next lease renewal runs within 1.5 s
    await asyncio.sleep(2)
    stop.set()
    await asyncio.gather(*tasks, return_exceptions=True)

    ok = True
    for b in bots:
        took = f"{b.resumed_at - t0:.1f} s" if b.resumed_at else "never"
        print(
            f"  {b.user}: now on {b.server}, drops {b.drops}, "
            f"full welcomes after the drop {b.welcomes}, "
            f"resumed after {took}, seq gaps/duplicates {b.gaps()}"
        )
        ok &= bool(b.resumed_at) and b.server != owner and not b.gaps()
    for b in drawers:
        # A resume replays the private state too: each drawer has its word again.
        words = [p for p in b.private if p["type"] == "your_word"]
        print(f"  drawer {b.user}: your_word {len(words)} times")
        ok &= len(words) >= 2
    worst = max((b.resumed_at or 1e9) - t0 for b in bots)
    print(f"recovery (slowest bot): {worst:.1f} s" if ok else "FAILED")
    if mode == "pause":
        logs = subprocess.run(
            ["docker", "logs", "--since", "30s", f"doodle-arena-{owner}-1"],
            capture_output=True,
            text=True,
        ).stdout
        fenced = sum(
            "lease lost" in line or "taken over by another server" in line
            for line in logs.splitlines()
        )
        print(f"zombie {owner}: {fenced} fencing log lines")
        ok &= fenced > 0
    if mode == "kill":
        # docker start, not compose: compose would need the main checkout's .env (a worktree has
        # none), and failing quietly here left the stack with one game server.
        subprocess.run(
            ["docker", "start", f"doodle-arena-{owner}-1"], check=True, capture_output=True
        )
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    asyncio.run(
        main(
            sys.argv[1] if len(sys.argv) > 1 else "kill",
            int(sys.argv[2]) if len(sys.argv) > 2 else 3,
        )
    )
