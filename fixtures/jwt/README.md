# Shared JWT verification fixtures

ADR 0001 has every service verify access tokens itself. These files make sure all of them apply
the same rules: each service's tests load `jwks.json` as the trusted key set and run every case in
`cases.json` (valid tokens must verify with the given `sub` and `roles`; every other case must be
rejected). Users today: auth (`services/auth/tests/test_jwt_fixtures.py`) and lobby
(`services/lobby/internal/authn`). The marketplace joins in Stage 4.

Regenerate (fresh keys every time; no private key is ever written):

```bash
(cd services/auth && uv run python -m tests.make_jwt_fixtures)
```
