# proto/: shared contracts

Protobuf definitions for everything that crosses a service boundary: Kafka events and gRPC. Both tracks depend on these files, so a change needs human approval and review (`.claude/rules/contracts.md`).

## Layout

| Path | What |
|---|---|
| `doodle/events/v1/` | Kafka events: `EventMeta` (`meta.proto`) and one file per topic, each with that topic's wrapper message |
| `doodle/game/v1/` | `ControlService` (lobby → game server), `GameMode`, and `StrokeLog`, the claim-check object in the `round-logs` bucket |
| `doodle/lobby/v1/` | `RegistryService` (game server → lobby heartbeats) |
| `buf.yaml` | Lint (`STANDARD` plus `COMMENTS`) and breaking-change (`FILE`) rules |
| `buf.gen.yaml` | Pinned remote plugins and where their output goes |
| `gen/` | Generated code, committed. Never edit it by hand, except the build files listed below |

## Rules

- **One wrapper per topic** (ADR 0004).
  - Every Kafka message is its topic's wrapper, for example `DrawingEvent` on `assets.drawings.v1`, with one `oneof event`. The topic-to-wrapper mapping is the event catalog in `docs/design-doc.md`, section 6.2.
  - A new event type is a new `oneof` case.
  - A consumer that sees an unset or unknown case logs it, counts it, commits the offset and moves on.
- **Every event starts with `EventMeta meta = 1`.**
- **Changes are additive.** Never reuse, renumber or retype a field. Mark removed fields and names as `reserved`. CI runs `buf breaking` against `master`.
- **Every message, field, enum, value, oneof, service and RPC has a comment** that says what it means and who produces and consumes it. `buf lint` enforces this.
- **Types:** IDs are `string`, times are `google.protobuf.Timestamp`, coins are `int64` (never floating point), and enums start with `*_UNSPECIFIED = 0`.

## Generating code

```bash
make proto              # format check, lint, regenerate, then build and test the code in all three languages
make test s=proto       # the same checks without regenerating
```

`make proto` is deterministic: running it on a clean tree changes nothing, and CI relies on that. It needs network access, because the plugins run on the Buf Schema Registry. Plugin versions are pinned in `buf.gen.yaml`, and the runtime library versions in `gen/*` must match them.

## Where generated code lands and how to use it

| Language | Path | Package | Use it from a service |
|---|---|---|---|
| Go | `gen/go/` (module `github.com/Bowenn/doodle-arena/proto/gen/go`) | `.../proto/gen/go/doodle/events/v1` | `require` the module and add `replace github.com/Bowenn/doodle-arena/proto/gen/go => ../../proto/gen/go`. See `tools/eventctl/go.mod` |
| Java | `gen/java/src/main/java` (Gradle build `doodle-proto`) | `com.doodlearena.proto.doodle.events.v1` | `includeBuild("../../proto/gen/java")` in `settings.gradle.kts`, then `implementation("com.doodlearena:doodle-proto")` |
| Python | `gen/python/doodle/` (package `doodle-proto`, with `.pyi` stubs) | `doodle.events.v1.rounds_pb2` | `uv add --editable ../../proto/gen/python` |

Hand-written files under `gen/`, which `make proto` keeps: `gen/go/go.mod` and `go.sum`, `gen/java/build.gradle.kts` and `settings.gradle.kts`, and `gen/python/pyproject.toml`, `uv.lock` and `tests/`.

Docker builds of services that import contracts (lobby and game-server so far) use the repository root as the build context, so they can copy `gen/go`. A `Dockerfile.dockerignore` next to each such Dockerfile sends only `proto/gen/go` and the service's own folder. Other services still build from their own folder and move to the root when they first import contracts.

## Looking at events

Kafka tools show protobuf as raw bytes. Use the decode helper instead:

```bash
./scripts/fake-events.sh                                     # publish a fake round and match
(cd tools/eventctl && go run . decode -topic game.rounds.v1 -n 3)
```

## Cross-language check

`tools/eventctl` writes golden files (`fixtures/golden/*.binpb`) from the JSON fixtures, using the Go code. `gen/python/tests/test_golden.py` decodes them and compares them with the same fixtures. If you change a fixture, regenerate the golden files with `go test -run TestGoldenFiles -update` in `tools/eventctl`.
