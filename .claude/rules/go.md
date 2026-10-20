---
paths:
  - "services/lobby/**"
  - "services/game-server/**"
  - "services/notifier/**"
  - "libs/go/**"
---

# Go services

- One Go module per service. Layout: `cmd/<service>/main.go`, with private code under `internal/`.
- Code more than one service needs lives once in `libs/go` (its own module: `authn`, `logging`, `kafkaauth`, `redistest`), reached through a `replace` directive like `proto/gen/go`. Never copy a package into another service; a change there runs every Go service's CI.
- Pass `context.Context` as the first parameter of anything that does I/O, and set a deadline on every outbound call (gRPC, HTTP, Redis, Kafka).
- Run tests with `go test -race ./...`. Prefer table-driven tests. Use Testcontainers for Redis, Kafka and Postgres integration tests.
- Log with `log/slog` and a JSON handler that adds `trace_id` and `span_id` from the context.
- Instrument with OpenTelemetry (`otelhttp`, `otelgrpc`), and add manual spans for room events and webhook deliveries.
- Every goroutine has an owner and an exit path. On SIGTERM: stop accepting work, drain, then exit.
- Wrap errors with `%w`. No global mutable state except configuration.
- Clients: `go-redis` for Redis and a pure-Go Kafka client such as `franz-go`, unless an ADR says otherwise.
