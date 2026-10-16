---
paths:
  - "proto/**"
---

# Contracts (proto/)

- Packages: `doodle.events.v1` for Kafka events, `doodle.lobby.v1` and `doodle.game.v1` for gRPC.
- Every event message has `doodle.events.v1.EventMeta meta = 1` (event_id, event_type, schema_version, occurred_at, producer).
- Changes are additive. Never reuse, renumber or change the type of a field; mark removed fields as `reserved`.
- Topic names follow `<domain>.<entity>.v1`. Every topic is listed in the event catalog in `docs/design-doc.md`.
- Comment every message and field: what it means, who produces it and who consumes it.
- Lint and generate with Buf (`make proto`). CI runs a breaking-change check. `proto/README.md` explains where generated code lands for Go, Java and Python.
- A contract change needs human approval, because both tracks depend on it.
