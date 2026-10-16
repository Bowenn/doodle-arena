# ADR 0002: How a WebSocket reaches the game server that owns its room

- Status: Accepted
- Date: 2026-10-15 (decided 2026-10-16)
- Deciders: Bowen

## Context

Each room lives on exactly one game server (the lease holder). A player's WebSocket must reach that server, and after a failover it must reach the new one.

## Options

### Option A: the lobby returns a direct URL

The join response contains a URL that names the server, for example `/ws/<server_id>/rooms/<room_id>`, and the gateway maps `server_id` to that instance. After a failover, clients ask the lobby again and get the new URL.

- Pros: explicit and easy to debug; always agrees with the lease; easy to explain.
- Cons: the gateway needs a mapping from server ids to instances; clients make an extra lobby call after a failover.

### Option B: the gateway routes by room id with consistent hashing

The URL contains only the room id, and Nginx picks the instance by hashing it (`hash ... consistent`). After a failover, the hash ring changes and remaps the room.

- Pros: no lobby call on reconnect; a good consistent-hashing story.
- Cons: the hash ring and the lease can disagree during changes, so a client can land on a server that doesn't own its room; that server must then redirect or proxy, which adds complexity.

## Decision

Option A: the lobby returns a direct URL, `/ws/<server_id>/rooms/<room_id>`.

- The game server reports its gateway path (`ws_address`, for example `/ws/game-server-1`) in every heartbeat, and the lobby appends `/rooms/<room_id>` (task 2.1). The lobby accepts only a path under `/ws`, never a full URL.
- Nginx maps `server_id` to an upstream through an explicit `map`, like the `/healthz/<service>` targets. An unknown id gets 404. The captured path segment is never used as a host, because that would let a client reach any internal service (SSRF).
- The game server checks that `server_id` in the path is its own and returns 404 otherwise, which catches a stale route.
- Join tickets are single-use (ADR 0005), so every reconnect already asks the lobby for a new ticket. Option B's main benefit, skipping that call, doesn't exist here, while its failure mode (the hash ring and the lease disagree) would need redirect or proxy code.

## Consequences

- The URL always agrees with the lease holder the lobby knows about, and a misrouted connection is visible in the path.
- Adding a game-server instance means adding a line to the gateway's map (locally), or a target group rule (AWS, ADR 0003). With many instances this mapping should come from service discovery instead of a static file.
- After a failover, clients reach the new server only through the lobby, so the lobby is on the reconnect path (it already is, because of single-use tickets).
- Interview angle: why consistent hashing loses its appeal once the client must talk to a coordinator anyway, and what the hash ring and the lease disagreeing would mean (a server that doesn't own the room receives the socket).
