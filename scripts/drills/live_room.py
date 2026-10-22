"""Live-room drill (task 6.1, ADR 0017): two bots play a live room through the gateway while
viewers watch it through the relays. It checks the things that make a relay correct:

  - every viewer gets the same events as the players, in seq order, with none missing;
  - no viewer sees the word before round_ended;
  - a viewer that reconnects with its last seq resumes on a relay without a full welcome;
  - a player's ticket can't watch, and a viewer's ticket can't play;
  - viewers guess (ADR 0019): wrong guesses are answered privately and, when the analytics
    profile runs, come back as top guesses; when every viewer guesses the word at once, the
    room announces exactly one of them (viewer_correct) to players and viewers; a viewer who
    got it can't guess again, and a player watching their own room can't guess as a viewer.

It also prints how much later a viewer gets an event than a player (the relay's hop).

Run it from the repo root against a running stack (make up). Access tokens are signed with the
local development key in secrets/, through auth's own TokenIssuer:

    uv run --project services/auth --with websockets python scripts/drills/live_room.py [viewers] [--hold s]

--hold keeps the drawer drawing for that many seconds (at most 150) before the round is guessed,
so the room can be watched in a browser meanwhile (the lobby's "Live now").

In a worktree, set DRILL_JWT_KEY to the main checkout's secrets/jwt-private.pem.
"""

import asyncio
import contextlib
import itertools
import json
import math
import os
import statistics
import sys
import time
import urllib.request
from datetime import UTC, datetime, timedelta
from pathlib import Path

import websockets

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
    req = urllib.request.Request(
        GW + path, method=method, data=json.dumps(body or {}).encode()
    )
    token = issuer.issue(user, ["player"], datetime.now(UTC), name=user)[0]
    req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read())


class Socket:
    """A player or a viewer: every message, with when it arrived."""

    def __init__(self, user, room_id, watch):
        self.user, self.room_id, self.watch = user, room_id, watch
        self.msgs = []  # (monotonic time, message)
        self.ws = None
        self.server = None

    async def connect(self, last_seq=0, ticket=None, ws_url=None):
        if ticket is None:
            verb = "watch" if self.watch else "join"
            j = call("POST", f"/api/lobby/rooms/{self.room_id}/{verb}", self.user)
            ticket, ws_url = j["ticket"], j["ws_url"]
        self.server = ws_url.split("/")[2]
        self.ws = await websockets.connect(WS + ws_url, origin=ORIGIN, max_size=1 << 24)
        await self.ws.send(
            json.dumps(
                {"type": "hello", "data": {"ticket": ticket, "last_seq": last_seq}}
            )
        )

    async def send(self, typ, data):
        await self.ws.send(json.dumps({"type": typ, "data": data}))

    async def run(self):
        # Ping every 5 s like the web app: a socket silent for 60 s is closed as idle.
        async def ping(ws):
            with contextlib.suppress(websockets.ConnectionClosed):
                while True:
                    await asyncio.sleep(5)
                    await ws.send(
                        json.dumps(
                            {"type": "ping", "data": {"t": int(time.time() * 1000)}}
                        )
                    )

        pinger = asyncio.create_task(ping(self.ws))
        with contextlib.suppress(websockets.ConnectionClosed):
            async for raw in self.ws:
                self.msgs.append((time.monotonic(), json.loads(raw)))
        pinger.cancel()

    def events(self):
        return [(t, m["data"]) for t, m in self.msgs if m["type"] == "event"]

    def seqs(self):
        out = []
        for _, m in self.msgs:
            if m["type"] == "welcome":
                out = [m["data"]["seq"]]
            elif m["type"] == "event":
                out.append(m["data"]["seq"])
        return out

    def private(self, typ):
        return [m["data"] for _, m in self.msgs if m["type"] == typ]


def gaps(seqs):
    return [(a, b) for a, b in itertools.pairwise(seqs) if b != a + 1]


async def closed_code(ws):
    try:
        await asyncio.wait_for(ws.recv(), 5)
    except websockets.ConnectionClosed as e:
        return e.rcvd.code if e.rcvd else None
    return None


async def main(n_viewers, hold):
    ok = True
    room = call(
        "POST",
        "/api/lobby/rooms",
        "live-host",
        {
            "live": True,
            "settings": {"draw_seconds": int(max(60, hold + 40)), "choose_seconds": 10},
        },
    )
    rid = room["room_id"]
    print(f"live room {rid}")
    host, guest = Socket("live-host", rid, False), Socket("live-guest", rid, False)
    await host.connect()
    tasks = [asyncio.create_task(host.run())]
    await asyncio.sleep(0.3)
    await guest.connect()
    tasks.append(asyncio.create_task(guest.run()))
    viewers = [Socket(f"live-viewer-{i}", rid, True) for i in range(n_viewers)]
    for v in viewers:
        await v.connect()
        tasks.append(asyncio.create_task(v.run()))
    await asyncio.sleep(1)
    print("viewers on " + ", ".join(sorted({v.server for v in viewers})))

    # A player's ticket can't watch; a viewer's ticket can't play.
    j = call("POST", f"/api/lobby/rooms/{rid}/join", "live-crossover")
    relay = viewers[0].server
    wrong = await websockets.connect(f"{WS}/ws/{relay}/rooms/{rid}", origin=ORIGIN)
    await wrong.send(json.dumps({"type": "hello", "data": {"ticket": j["ticket"]}}))
    code = await closed_code(wrong)
    print(f"  a player's ticket on a relay: close {code}")
    ok &= code == 4001
    w = call("POST", f"/api/lobby/rooms/{rid}/watch", "live-crossover")
    wrong = await websockets.connect(WS + host_url(host, rid), origin=ORIGIN)
    await wrong.send(json.dumps({"type": "hello", "data": {"ticket": w["ticket"]}}))
    code = await closed_code(wrong)
    print(f"  a viewer's ticket on the game server: close {code}")
    ok &= code == 4001

    await host.send("start", {})
    drawer = None
    for _ in range(100):
        drawer = next((p for p in (host, guest) if p.private("word_options")), None)
        if drawer:
            break
        await asyncio.sleep(0.1)
    if drawer is None:
        sys.exit("no drawer got word options: is the stack healthy?")
    await drawer.send("choose_word", {"index": 0})
    await asyncio.sleep(0.5)
    word = drawer.private("your_word")[0]["word"]
    if hold:
        print(f"drawing for {hold:.0f} s: watch it at {GW}/rooms/{rid}/watch")
    for i in range(max(40, int(hold / 0.05))):
        await drawer.send(
            "stroke_batch",
            {
                "stroke_id": f"s{i}",
                "points": spiral(i),
                "color": "#112233",
                "width": 3,
                "done": True,
            },
        )
        await asyncio.sleep(0.05)
        if i == 20:
            # One viewer drops and resumes with its last seq, through the lobby.
            v = viewers[0]
            last = v.seqs()[-1]
            await v.ws.close()
            await v.connect(last_seq=last)
            tasks.append(asyncio.create_task(v.run()))
    ok &= await viewer_guesses(viewers, drawer, rid, word)
    guesser = guest if drawer is host else host
    await guesser.send("guess", {"text": word})
    for _ in range(100):  # the round ends once everyone guessed
        if any(e["type"] == "round_ended" for _, e in host.events()):
            break
        await asyncio.sleep(0.1)
    await asyncio.sleep(1)
    for s in [host, guest, *viewers]:
        await s.ws.close()
    await asyncio.gather(*tasks, return_exceptions=True)

    player_seqs = host.seqs()
    player_at = {e["seq"]: t for t, e in host.events()}
    lag = []
    announced = [e for _, e in host.events() if e["type"] == "viewer_correct"]
    seen = [any(e["type"] == "viewer_correct" for _, e in v.events()) for v in viewers]
    print(
        f"  viewer_correct: {len(announced)} announced "
        f"({announced[0]['data']['name'] if announced else '-'}), seen by {sum(seen)}/{len(viewers)} viewers"
    )
    ok &= len(announced) == 1 and all(seen)
    ok &= bool(announced) and announced[0]["data"]["user_id"] in {
        v.user for v in viewers
    }
    for v in viewers:
        seqs = v.seqs()
        early = [
            e["type"]
            for _, e in v.events()
            if word in json.dumps(e) and e["type"] != "round_ended"
        ]
        welcomes = len(v.private("welcome"))
        resumed = len(v.private("resumed"))
        missing = sorted(set(player_seqs) - set(seqs) - {player_seqs[0]})
        lag += [t - player_at[e["seq"]] for t, e in v.events() if e["seq"] in player_at]
        print(
            f"  {v.user} on {v.server}: {len(seqs)} seqs, gaps {gaps(seqs)}, "
            f"missing {missing[:5]}, welcomes {welcomes}, resumes {resumed}, "
            f"word early in {early}"
        )
        ok &= not gaps(seqs) and not missing and not early
        if v is viewers[0]:
            ok &= welcomes == 1 and resumed == 1
    if lag:
        lag_ms = sorted(1000 * x for x in lag)
        print(
            f"viewer behind player: median {statistics.median(lag_ms):.1f} ms, "
            f"p99 {lag_ms[int(0.99 * (len(lag_ms) - 1))]:.1f} ms over {len(lag_ms)} events"
        )
    print("OK" if ok else "FAILED")
    sys.exit(0 if ok else 1)


async def viewer_guesses(viewers, drawer, rid, word):
    """Viewers guess wrong, then all guess the word at once (ADR 0019). Returns whether every
    check passed; the announcement itself is checked after the round."""
    ok = True
    mark = [len(v.msgs) for v in viewers]

    def answers(v, i, typ):
        return [m["data"] for _, m in v.msgs[mark[i] :] if m["type"] == typ]

    wrong = ["apple", "banana", "cherry", "apple", "banana", "apple"]
    for i, v in enumerate(viewers):
        await v.send("guess", {"text": wrong[i % len(wrong)]})
    await asyncio.sleep(1)
    results = [answers(v, i, "guess_result") for i, v in enumerate(viewers)]
    right = sum(1 for r in results if r and not r[0]["correct"])
    print(f"  wrong viewer guesses answered privately: {right}/{len(viewers)}")
    ok &= right == len(viewers)

    # The top guesses come back from analytics (ADR 0018) when its profile runs. Event time only
    # moves with new guesses, so one viewer keeps guessing (the bucket refills 1 a second).
    if analytics_running():
        top = None
        for k in range(25):
            await viewers[-1].send("guess", {"text": f"tick{'x' * (k % 3)}"})
            await asyncio.sleep(1.1)
            top = next((t for v in viewers for t in v.private("top_guesses")), None)
            if top:
                break
        print(
            f"  top guesses on the watch page: {top['guesses'] if top else 'none in 27 s'}"
        )
        ok &= bool(top) and any(g["text"] == "apple" for g in top["guesses"])
        ok &= not any(
            word in json.dumps(t) for v in viewers for t in v.private("top_guesses")
        )
    else:
        print(
            "  top guesses: skipped (start the analytics profile: PROFILES=live,analytics make up)"
        )

    # A player watching their own room is refused; the drawer knows the word.
    me = Socket(drawer.user, rid, True)
    await me.connect()
    me_task = asyncio.create_task(me.run())
    await asyncio.sleep(0.5)
    await me.send("guess", {"text": word[:-1]})
    await asyncio.sleep(0.5)
    refused = [e for e in me.private("error") if e["code"] == "player"]
    print(f"  the drawer guessing as a viewer: {'refused' if refused else 'ACCEPTED'}")
    ok &= bool(refused)
    await me.ws.close()
    await asyncio.gather(me_task, return_exceptions=True)

    # Everyone at once: the room's owner decides who was first.
    await asyncio.gather(*(v.send("guess", {"text": word.upper()}) for v in viewers))
    await asyncio.sleep(1.5)
    correct = sum(
        1
        for i, v in enumerate(viewers)
        if {"correct": True} in answers(v, i, "guess_result")
    )
    await viewers[0].send("guess", {"text": "again"})
    await asyncio.sleep(0.5)
    again = [e["code"] for e in answers(viewers[0], 0, "error")]
    print(
        f"  the word from every viewer: {correct}/{len(viewers)} correct; a second guess: {again}"
    )
    ok &= correct == len(viewers) and "already_guessed" in again
    return ok


def analytics_running():
    try:
        with urllib.request.urlopen(
            "http://localhost:8081/jobs/overview", timeout=2
        ) as r:
            return any(j["state"] == "RUNNING" for j in json.loads(r.read())["jobs"])
    except OSError:
        return False


def spiral(i):
    """A few points of a growing spiral: something to look at while watching."""
    r = min(300 + 12 * i, 4500)
    return [
        [
            int(5000 + r * math.cos((i + k / 4) / 3)),
            int(5000 + r * math.sin((i + k / 4) / 3)),
            k,
        ]
        for k in range(4)
    ]


def host_url(sock, rid):
    return f"/ws/{sock.server}/rooms/{rid}"


if __name__ == "__main__":
    args = sys.argv[1:]
    hold = 0.0
    if "--hold" in args:
        i = args.index("--hold")
        hold = min(float(args[i + 1]), 150)
        del args[i : i + 2]
    asyncio.run(main(int(args[0]) if args else 6, hold))
