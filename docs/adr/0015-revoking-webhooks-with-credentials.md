# ADR 0015: Revoking webhooks with the credentials

- Status: Accepted
- Date: 2026-10-18
- Deciders: Bowen

## Context

When auth sees a rotated refresh token come back, someone else had the session. Since task 4.7 it revokes the session family and the user's API keys. Two channels outlived that:
- The user's webhook endpoints, made with a key or (since 4.8) with a session, kept receiving the user's sales and Stripe credits. A thief who registered one kept watching the account.
- The thief's access token is a JWT that stays valid for up to 15 minutes. It was enough to register a new endpoint after the theft was detected.

The notifier couldn't act on either, because nothing told it about the revocation. Found in the Stage 4 review (D43).

## Options

### 1. How the notifier learns about a revocation

- **A. An auth event over Kafka:** `CredentialsRevoked` on a new topic `auth.users.v1`, written to an outbox in the revoking transaction and published by a relay.
  - Pros: the system's usual pattern; nothing is lost if Kafka or the notifier is down; other services can subscribe later.
  - Cons: auth gains an outbox, a relay process and a Kafka producer.
- **B. The notifier asks auth on every delivery**, through an internal endpoint like the API-key check, with a cache.
  - Pros: no new topic.
  - Cons: a call to auth in the delivery path; an auth outage stalls webhooks; the 15-minute access-token gap stays open.
- **C. Auth calls the notifier synchronously** when it revokes.
  - Pros: immediate.
  - Cons: a revocation is lost if the notifier is down; auth depends on the notifier's availability.

### 2. The thief's still-valid access token

- **A. A watermark:** the notifier stores the newest revocation time per user and refuses any credential issued at or before it: a session token by its `iat`, an API key by its `created_at`.
- **B. Rely on disabling endpoints:** a thief could register a new endpoint for up to 15 minutes after detection.

### 3. Getting a disabled endpoint back

- **A. Enable rotates the secret:** the old secret may be known to the thief.
- **B. Delete and register again:** the endpoint id and its delivery log are lost.

### 4. Auth's relay

- **A. A separate `auth-relay` process from the same image**, like `assets-relay`.
- **B. A thread inside the web process**, like the marketplace's relay.

## Decision

1A, 2A, 3A, 4A.

- **Auth's side:**
  - `CredentialsRevoked {meta, user_id, reason, revoked_at}` goes to `auth.users.v1`, keyed by the user.
  - It is written in the transaction that revokes the family and the keys, **once per family**: a `reuse_handled_at` marker stops repeated replays of an old token from revoking the user's new keys and disabling their webhooks again and again.
  - The `auth-relay` process publishes it at least once.
- **The notifier's consumer:**
  - It reads the topic in its own group, `notifier-auth`, **from the earliest offset**. The main group starts at the newest offset, which would skip revocations published before its first subscribe; revocations are few and idempotent, so reading them all is cheap.
  - One transaction raises the user's watermark (`GREATEST`), disables all the user's live endpoints and cancels their pending deliveries.
  - An event that isn't newer than the watermark changes nothing, so a replay never disables an endpoint the owner enabled again.
- **The API:**
  - Every credential issued at or before the watermark is refused with 401: a session by `iat`, a key by `created_at`. Auth's verify now returns `created_at`, because the notifier caches valid keys for 30 seconds.
  - Registering and enabling re-check the watermark under the owner's advisory lock, which the revocation also takes. So a request that passed the first check can't commit an endpoint after the revocation was applied.
- **Auth's own watermark:** the theft transaction sets `users.credentials_revoked_at`, and the API-key routes refuse an access token issued at or before it (`401 TOKEN_REVOKED`). Without this, the thief's still-valid token could mint a fresh key whose `created_at` passes the notifier's watermark. Key creation re-checks it under the user's advisory lock, which the theft response takes too, so a key request can't slip in while the revocation commits.
  - `POST /api/notifier/endpoints/{id}/enable` makes a disabled endpoint live with a new secret, shown once.

## Consequences

- **What the thief loses:** every webhook channel stops within about 0.15 s of detection (measured: refresh, outbox, relay, notifier). A thief's token or cached key can't open a new one.
- **What the user goes through:** they are signed out (as before) and see their endpoints as disabled, with Enable. Endpoints they create with a fresh session in the moment before the event arrives are disabled too; Enable fixes that.
- **Auth's new parts:** auth now owns an outbox and a Kafka producer. Its image builds from the repository root (for the generated contracts), and an `auth-relay` container joins the stack.
- **Clock precision:** the watermark compares `iat` in whole seconds against `revoked_at`. A new sign-in in the same second as the revocation is refused once, which errs on the safe side.
- **Out of scope:** other services still accept the thief's access token until it expires (15 minutes). Revoking JWTs elsewhere would need this watermark in every verifier, and isn't needed for anything that leaks data the way webhooks do.
