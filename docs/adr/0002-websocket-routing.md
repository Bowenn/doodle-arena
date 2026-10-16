# ADR 0002: How a WebSocket reaches the game server that owns its room

- Status: Proposed (decide in Stage 2, before task 2.5)
- Date: 2026-10-15
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

Suggestion for discussion: Option A for v1. Option B can be discussed in interviews as the alternative and its failure mode.

## Decision

(empty until decided)

## Consequences

(fill in once decided)
