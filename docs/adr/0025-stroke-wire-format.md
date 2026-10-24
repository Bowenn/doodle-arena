# ADR 0025: Stroke batches: 50 ms and JSON by default, protobuf frames as an opt-in subprotocol

- Status: Accepted (2026-10-23)
- Date: 2026-10-23
- Deciders: Bowen

## Context

D16 kept the WebSocket protocol in JSON for v1 and left a binary format and the batch interval to a Stage 6 experiment (task 6.4). Strokes are almost all of the traffic. Every drawer sends a batch every 50 ms; the room commits each batch to Redis (one Lua call) and sends it to every other player. A room's event log in Redis is JSON, and relays and viewers read it as it is.

Decided with the plan (Bowen):

- Protobuf in `proto/`.
- Only stroke batches go binary, negotiated per socket.
- Both formats are kept, and this ADR sets the defaults from measurements.
- The runs happen on the dev box with each game server limited to one CPU.

## What was built

- **The contract:** `doodle.game.v1.StrokeFrame` (`ws.proto`), with the client's `batch` and the room's `event`. Points are packed varints of flat `x, y, t_ms` triples.
- **Negotiation:** a socket that asks for the subprotocol `doodle.strokes.pb` sends and receives stroke batches as binary frames; every other message stays JSON. Without the subprotocol nothing changed, and a binary frame still closes with 1003.
- **Encoding:** the room encodes each stroke event once per format its sockets use. Welcome, resume replays, relays, viewers and the Redis log stay JSON.
- **Clients:** the web app (`?wire=pb`) and k6 have small hand-written codecs, checked against bytes the game server writes into a fixture.
- **The experiment:** `make experiment-strokes` runs the matrix and the ceiling (docs/performance.md, section 12).

## Results (GCP dev box, e2-standard-8; game servers at one CPU each)

24 rooms (96 players, 48 drawers at 160 points a second) held for 3 minutes, medians of two runs.

| Batch | Ink delay p50 / p95 (JSON) | Game-server CPU (JSON) | Redis commands/s (JSON) | Server CPU per 1,000 batches/s, JSON / protobuf | Stroke bytes per delivered event, JSON / protobuf |
|---|---|---|---|---|---|
| 16 ms | 14 / 33 ms | 1.16 cores | 5,235 | 0.65 / 0.67 | 267 / 81 B |
| 33 ms | 30 / 42 ms | 0.91 | 3,570 | 0.76 / 0.85 | — |
| 50 ms | 47 / 53 ms | 0.66 | 2,460 | 0.81 / 0.89 | — |
| 100 ms | 96 / 101 ms | 0.38 | 1,330 | 0.92 / 0.97 | — |

- **Batch interval:**
  - **Ink delay** is from a point being drawn until another player has it, so a batch's oldest point waits about one interval.
  - **Cost** follows batches per second: each one is a Redis commit and a fan-out.
  - **The trade:** 33 ms buys 17 ms of ink delay for 38% more server CPU and 45% more Redis commands; 16 ms buys 33 ms for 76% and 113%.
- **Format:**
  - **Bytes:** protobuf cut stroke bytes by about 70% both ways: 81 against 267 bytes per event delivered, 48 against 148 per batch sent.
  - **Server CPU:** it did not cut CPU. Per batch the server spent 3–12% more with protobuf, because the event log stays JSON and each stroke is marshalled twice. Decoding protobuf is cheaper, but a batch's cost is mostly its Redis commit and its fan-out.
  - **Browser CPU:** in V8 both cost microseconds per message. Receiving one event takes 0.50 µs to decode and 0.67 µs with `JSON.parse`; sending a batch takes 2.4 µs to encode and 0.46 µs with `JSON.stringify`.
- **Ceiling (JSON, 50 ms):** stroke fan-out p95 stays under 250 ms up to 80 rooms on two one-CPU game servers (184 ms), then 269 ms at 100 rooms. Accepted batches level off near 2,000 a second.
- **What couldn't be measured:**
  - The protobuf ceiling: k6's JavaScript interpreter tops out at about 900 binary batches a second.
  - A first run at 40 rooms was misleading for the same reason. In protobuf mode k6 used twice the CPU, its drawing timers ran late and it sent 22% fewer batches, which made protobuf look both faster on the server and slower on the client. The runs now record k6's CPU and the server's own count of batches, and compare formats per batch.

## Decision

- **The batch interval stays 50 ms.** The others see a drawing about 50 ms late on top of the network, while the drawer sees their own stroke at once (the local overlay). Shorter intervals pay for each millisecond with Redis commits and CPU, roughly a quarter fewer rooms per server at 33 ms.
- **JSON stays the default format, and protobuf stays an opt-in subprotocol.**
  - **Against switching:** it saves bandwidth, but not server CPU, and two codecs must stay in step.
  - **When to revisit:**
    - if player bandwidth becomes the constraint (mobile data, AWS egress), switch the web app's default with `VITE_STROKE_WIRE=pb`;
    - or if the event log moves to protobuf too, which would remove the double encoding.

## Options considered

- **Protobuf for every message:** rejected for the experiment. Strokes are nearly all the traffic, and the rest would double the codec work for little.
- **A hand-rolled compact format (delta-encoded varints):** smaller still, but a format of our own to specify and fuzz. Protobuf keeps the contract in `proto/` with Buf's checks.
- **Making 33 ms the default:** rejected above; the cost per room is too high for 17 ms.

## Consequences

- The game server accepts `doodle.strokes.pb`, and the metrics `game.ws.frames`, `game.ws.bytes` and `game.encode.duration` show traffic per format.
- `make experiment-strokes` can be repeated in AWS (task 6.9). A Go load client, rather than k6, would be needed to find protobuf's server ceiling.
