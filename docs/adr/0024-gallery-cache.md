# ADR 0024: Gallery cache: cache-aside in Redis with a generation key and single-flight fills

- Status: Accepted (2026-10-23)
- Date: 2026-10-23
- Deciders: Bowen

## Context

Task 6.7b adds a cache to the assets gallery. D43 (the Stage 4 review) already decides where: Redis cache-aside in assets, where reads dominate and the data rarely changes. It does not go on listings or wallets, whose answers must be exact.

Three endpoints read drawings on every page view, each a Postgres query (the replica since ADR 0020):

- `GET /api/assets/drawings`: the gallery, newest first, keyset pages.
- `GET /api/assets/drawings?ids=`: every Market page, 20 ids at a time.
- `GET /api/assets/drawings/{id}`: a drawing's page.

The facts that shape the design:

- **A decision is final.** The AI decides from `processing` and a moderator from `review` (`_DECIDE_FROM` in `drawings/moderation.py`). Nothing moves an approved drawing again, so an approved drawing's fields never change.
- **Only approved drawings are the same for everyone.** Lists filtered by `author_id` or `match_id` also show the caller's own drawings in any state.
- **Image URLs are presigned and expire** (15 minutes, reused for 10).
- **New approvals change the gallery's pages.** A drawing approved after a moderator's review keeps its creation-time id, so it can land on page 2 or 3, not only page 1.
- **Redis is shared with Celery's broker** (logical database 1, the `assets` user's `~assets:*`), with no eviction policy and AOF on.

## Decision

1. **Cache only approved drawings, by their fields, never their URLs.** `assets:cache:drawing:<id>` holds a drawing's fields as JSON (the image keys, not URLs) for 10 minutes. URLs are signed when the response is built, so a cached entry can't hand out an expired link, and a response is byte-identical with the cache on or off. Detail pages and `?ids=` lookups read these entries (`MGET`); misses use the existing query and store what's approved.
2. **Gallery pages carry a generation.** `assets:cache:gallery:<gen>:<limit>:<cursor or first>` holds the unfiltered list's first three pages for 60 seconds. `decide()` increments `assets:cache:gallery:gen` after an approval commits (`transaction.on_commit`). A reader reads the generation, then queries, then stores under the generation it read. A reader that raced an approval can only store under a generation nobody asks for any more, so the classic cache-aside race (a stale value stored after the invalidation) can't serve a stale page.
3. **Pages are filled from the primary.** A replica that hasn't replayed the approval yet would put the old page under the new generation. Misses are rare, so the primary barely notices.
4. **One fill per miss (single-flight).** The first request to miss takes `SET assets:cache:lock:<page key> <token> NX PX 2000`, fills the page and releases the lock with a compare-and-delete script. Others poll the page every 10 ms for up to 200 ms, then query Postgres themselves without storing.
5. **Fail open.** Redis calls time out after 100 ms. After any Redis error the cache is skipped for 5 seconds, so an outage costs one timeout, not one per request. Postgres stays the only truth, and `ASSETS_CACHE=off` turns the cache off (for measuring).
6. **No takedown action.** Approvals are final today. `cache.invalidate_drawing(id)` exists and is tested; a future takedown must call it and bump the gallery generation.

## Options considered

- **Freshness by TTL only.** It's simpler, with a higher hit rate under heavy play, but a new drawing is missing for up to the TTL, and "invalidation" would be expiry. Rejected.
- **Deleting page keys on approval.** The cache-aside race is still open: a reader that queried before the commit stores the old page after the delete. And with no `SCAN` (the ACL forbids it), every key would have to be tracked. The generation solves both. Rejected.
- **Serving the stale page while one request refreshes it.** No request waits, but the old value outlives the approval and needs a second key and state. Single-flight is easier to show correct in a test. Rejected.
- **A takedown action now.** It would reach the marketplace (a minted, perhaps listed drawing taken down) and needs its own design. Deferred.

## Consequences

- At one approval a second (heavy play), a gallery page lives about a second; single-flight keeps each turnover to one query.
- The cache adds keys to Celery's Redis. Every key has a TTL; an entry is about half a kilobyte.
- `assets.cache.requests`, `assets.cache.lock_waits` and `assets.cache.invalidations` show how it behaves, and AssetsCacheDegraded warns when Redis errors make the cache skip.
