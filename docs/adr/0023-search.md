# ADR 0023: Search: one indexer builds Elasticsearch documents from assets' own projection

- Status: Accepted (2026-10-23)
- Date: 2026-10-23
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.7 adds search over drawings and listings. The design doc already decides three things:

- Assets owns an Elasticsearch index `drawings`.
- It indexes approved drawings and the active listings announced on `market.listings.v1`.
- The API is `GET /api/assets/search`.

What it leaves open:

- how the index stays in sync with two services' data;
- which engine runs where;
- how the index is rebuilt.

The facts that constrain it:

- **Drawings belong to assets** (Postgres), and **listings belong to the marketplace**. Database per service: assets learns about listings only from events.
- **A listing ends in two ways.** `ListingCancelled` arrives on `market.listings.v1`, but a sale emits only `SaleCompleted`, on `market.sales.v1`. Events about one drawing arrive on three topics, so in no fixed order.
- **Every topic keeps Kafka's default 7 days.** Replaying the topics can't rebuild listings older than that.
- **The marketplace's listings API needs a user's token,** so a background job can't read it.
- **`DrawingApproved` has no `word`.** It carries the drawing's id, authors and images.

## Options

### How the index stays in sync

- **A. One indexer reading events, through a projection in assets' Postgres.**
  - A new assets process consumes `assets.drawings.v1`, `market.listings.v1` and `market.sales.v1`.
  - It applies each event to tables in assets' own database (the active listing per drawing, the closed listing ids, a version per drawing), then builds the drawing's whole document from that database and writes it to Elasticsearch.
  - Pros:
    - Every write to Elasticsearch is a full document from one source of truth, so a retry, a reorder or a rebuild converges.
    - A rebuild needs nothing outside assets.
  - Cons:
    - A new process.
    - Assets stores a copy of listing state (derived, its own).
- **B. Celery on commit for drawings, a consumer for listings.**
  - Pros: drawings are indexed without a Kafka hop.
  - Cons: two different paths write one document, and they can race.
- **C. Dual writes** from the request paths.
  - Pros: simplest.
  - Cons: a crash between Postgres and Elasticsearch leaves them disagreeing, with nothing to repair it.

### The engine and where it runs

- **A. Elasticsearch, self-hosted:** the official image, one node, security on with a role per service, in compose (an opt-in profile locally) and on the AWS data host.
- **B. OpenSearch, self-hosted:** the Apache-2.0 fork, the same shape.
- **C. Amazon OpenSearch Service in AWS:** managed, but a second engine next to the local one, plus its own VPC and IAM work and cost.

## Decision

Bowen chose **A and A**, with word search (typo tolerance and prefixes), filters, sorting and a price facet, and search boxes on the Market and the Gallery.

**The projection.** Assets' Postgres gains `search_listings` (one active listing per drawing), `search_closed_listings` (listing ids that were cancelled or sold) and a `search_version` on each drawing. Per event:

- `DrawingApproved` and `DrawingRejected` bump the version: the document must exist, or must not.
- `ListingCreated` sets the active listing, unless its id is already closed.
- `ListingCancelled` and `SaleCompleted` record the id as closed, and clear the active listing only if it is that one.

So the projection ends in the same state whatever order a drawing's events arrive in.

**The documents.**

- After the projection commits, the indexer builds the whole document from Postgres. It writes it with `version_type=external` and the drawing's `search_version`, so an older write (a retry, the rebuild) never overwrites a newer one.
- A drawing that isn't approved is deleted.
- If Elasticsearch is down, the record is retried and its offset isn't committed.

**Search.**

- A `bool` query: `multi_match` over the word, fuzzy on whole words and an edge n-gram subfield for prefixes, with the filters in `filter` context.
- `search_after` paging, and a range aggregation of listed prices.
- **Elasticsearch ranks, Postgres answers.** Results are hydrated from Postgres, which re-checks that each drawing is approved and signs fresh image URLs.

**The rebuild.** `manage.py reindex_search`:

1. Builds a new index from Postgres.
2. Swaps the alias `drawings` atomically.
3. Re-indexes what changed during the build.

It is safe while the indexer runs, because external versions refuse stale writes.

**The backfill.** `POST /api/market/admin/listings/republish` (admin only) writes a `ListingCreated` for every active listing to the marketplace's outbox. The indexer treats them like any other. Run it after the first deploy of search, and after `aws-up` restores a backup. No contract changes.

**The engine.**

- Elasticsearch 9.5: one node, security on, a 512 MB heap and a 1 GB memory cap.
- An init job creates the role `assets_search`, limited to the indices `drawings*`, and the user `assets`.
- Locally it is an opt-in compose profile, `search` (with the indexer). In AWS it runs on the data host.
- Assets' Kafka user gains Read on the three topics with the group `assets-search`.

## Consequences

- **Search is eventual:** a change shows up within seconds (design doc, section 10). Hydration keeps every answer truthful about what Postgres holds.
- **Assets holds derived listing state.** It is rebuilt from events and never read by anyone else, so the marketplace stays the only owner of listings.
- **The backfill is a manual step** after a restore. It's documented and idempotent: a cross-service read path would have been the alternative.
- **Memory:** Elasticsearch adds about 1 GB. It's opt-in locally; on the t4g.large data host the headroom is measured in task 6.9.
- **What to watch:**
  - search requests by result and their latency;
  - documents indexed by event and result;
  - the index lag (event time until indexed);
  - the `assets-search` consumer group's lag;
  - the alerts SearchIndexLagging and SearchUnavailable.
