# ADR 0001: Where JWTs are verified

- Status: Proposed (decide in Stage 1, task 1.3)
- Date: 2026-10-15
- Deciders: Bowen, Will

## Context

The auth service issues short-lived JWT access tokens signed with an asymmetric key, and publishes the public keys at `/api/auth/.well-known/jwks.json`. Every other service needs to know who is calling. The question is where the token is checked.

## Options

### Option A: verify at the gateway

Nginx calls auth for every request (`auth_request`), and forwards the user id and roles to services as trusted headers.

- Pros: one place to get right; services stay simpler.
- Cons: an extra hop on every request; services must trust headers, so anything that reaches a service without passing the gateway is unauthenticated; harder to explain as zero-trust.

### Option B: each service verifies the JWT itself

Each service fetches the JWKS, caches it, and verifies the signature, expiry, issuer and audience locally. The gateway only routes.

- Pros: no extra hop; services don't trust the network; standard practice for JWTs with public keys.
- Cons: verification code exists in three languages (well-supported libraries make this small); key rotation must be handled by caching the JWKS and refreshing it on unknown key ids.

Suggestion for discussion: Option B, with a shared test that checks all three languages reject expired, wrongly signed and wrong-audience tokens.

## Decision

(empty until decided)

## Consequences

(fill in once decided)
