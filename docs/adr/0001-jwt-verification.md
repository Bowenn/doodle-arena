# ADR 0001: Where JWTs are verified

- Status: Accepted
- Date: 2026-10-15
- Deciders: Bowen

## Context

The auth service issues short-lived JWT access tokens signed with an asymmetric key, and publishes the public keys at `/api/auth/.well-known/jwks.json`. Every other service needs to know who is calling. The question is where the token is checked.

Related questions that come with it: which signing algorithm and audience to use, how the Django admin (a server-rendered page that a browser navigates to, so it can't send an in-memory bearer token) authenticates moderators, and which auth endpoints must never be reachable through the gateway.

## Options

### Option A: verify at the gateway

Nginx calls auth for every request (`auth_request`), and forwards the user id and roles to services as trusted headers.

- Pros: one place to get right; services stay simpler.
- Cons: an extra hop on every request, and auth becomes a dependency of every request; services must trust headers, so anything that reaches a service without passing the gateway is unauthenticated, and the gateway must strip client-supplied identity headers; harder to explain as zero-trust. API keys would still need their own path.

### Option B: each service verifies the JWT itself

Each service fetches the JWKS, caches it, and verifies the signature, expiry, issuer and audience locally. The gateway only routes.

- Pros: no extra hop; services don't trust the network; standard practice for JWTs with public keys.
- Cons: verification code exists in three languages (well-supported libraries make this small); key rotation must be handled by caching the JWKS and refreshing it on unknown key ids.

## Decision

Option B: every service verifies access tokens itself.

- **Algorithm: RS256.** It is the default in every JWT library we use, including Spring Security's `NimbusJwtDecoder`, Python's `PyJWT`/`joserfc` and Go's `golang-jwt` / `lestrrat-go/jwx`. ES256 or EdDSA would give smaller tokens but add configuration in at least one language for no benefit we need.
- **Audience: a single value, `doodle-api`.** Every request comes from the same web app acting for the same user, so per-service audiences would force the app to hold several tokens (or put every audience into one token, which defeats the point) without reducing real risk. The 15-minute expiry limits the damage of a leaked token. Authorization uses the `roles` claim. If service-to-service tokens are ever needed, they get their own audiences then.
- **Checks in every service:** signature against the cached JWKS (refetched when an unknown `kid` appears, rate-limited), `iss`, `aud == doodle-api`, `exp` and `iat` with at most 30 seconds of clock skew, and only `RS256` accepted (never `none`, never HMAC). (Updated 2026-10-18: the cached JWKS also has a maximum age. Refetching only on an unknown `kid` meant a key that auth withdrew stayed trusted until the service restarted. The lobby, the notifier and assets now refresh the set after 10 minutes, keep using the last good set while a refresh fails, and refuse every token once the set is an hour old without a successful refresh, so blocking the fetch can't keep a withdrawn key alive. The marketplace's Spring decoder already expires its cache. The decision is unchanged.)
- **The gateway only routes.** It still refuses any request with an `X-User-*` or `X-Roles` header (400) as a safeguard, so nobody adds a trusted-header shortcut later. (Updated 2026-10-17: it first cleared only three named headers; it now refuses every one, through njs, because nginx can't clear headers by pattern. The decision is unchanged.)
- **Internal endpoints live on a separate internal port** that the gateway never routes. This covers the API-key check endpoint in auth (`/internal/api-keys/verify`) and every gRPC port. `/api/auth/*` on the public port serves only public endpoints.
- **Django admin: Google sign-in bridged into a Django session.** A moderator opens `/admin/`, which redirects to the normal Google login in auth. The web app then sends its access token once, in a `POST` body (never in a URL), to `/admin/sso/` in assets. Assets verifies the JWT with the same rules as every other service, requires the `moderator` or `admin` role, creates or updates a Django `User` with `is_staff` set, and logs it into a Django session. The session cookie is HttpOnly, `SameSite=Lax`, scoped to `/admin/`, and lasts at most 1 hour, so a revoked role stops working within the hour. Django's CSRF protection stays on. There are no Django passwords. (Updated 2026-10-17, task 3.1: the web app's `/moderate` route sends the token in an `Authorization: Bearer` header of that one `POST` instead of the body, through the same `authFetch` that every API call uses, so the token never leaves `auth.ts` and an expired one is refreshed and retried. It is still never in a URL. The `POST` carries a CSRF token that `GET /admin/sso/` hands out, and the `csrftoken` cookie is scoped to `/admin/` too. `/admin/` sends a signed-out moderator to `/moderate`, which shows the usual "Sign in with Google" button and comes back to the admin afterwards, rather than redirecting to Google at once. The decision is unchanged.)

## Consequences

- Each language needs a small verification module plus a shared test suite: every service must reject an expired token, a token signed by an unknown key, a token with the wrong `aud` or `iss`, and an `alg: none` or HS256 token. The test fixtures (keys and tokens) live in one place and are reused by all three languages.
- Key rotation: auth publishes the old and new keys in the JWKS during a rotation, and services refetch on an unknown `kid`. Rotation needs no coordinated restart. A withdrawn key stops being accepted within the cache's maximum age (10 minutes in the lobby, the notifier and assets).
- Auth isn't on the hot path of every request, so an auth outage blocks logins and refreshes but not requests that carry a still-valid token. That holds only if every service keeps its last good key set through the outage: the marketplace used Spring's defaults, with no such grace, until 2026-10-25; it now follows the shared rule (design doc 4.2) through Nimbus's key sources.
- The Django bridge is security-sensitive custom code (about 100 lines) and needs its own tests: wrong role, expired token, token sent by GET, CSRF.
- A role change takes effect at the next token refresh (at most 15 minutes) for APIs and at session expiry (at most 1 hour) for the admin.
- Interview angle: zero-trust between services; the trade-off against gateway verification; why one audience is enough here.
