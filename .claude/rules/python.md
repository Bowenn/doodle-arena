---
paths:
  - "services/auth/**"
  - "services/assets/**"
  - "services/ai/**"
---

# Python services

- Python 3.14 (pinned in each service's `.python-version`), with dependencies managed by `uv`. Lint and format with `ruff`; type-check with `mypy` or `pyright`.
- `auth` and `ai` use FastAPI with Pydantic models. `assets` uses Django (ORM and admin) with Celery workers and a Redis broker.
- Migrations: Alembic for `auth`, Django migrations for `assets`.
- Test with `pytest`. Integration tests use Testcontainers for Postgres, Redis, Kafka and MongoDB.
- Instrument with OpenTelemetry through the service's `telemetry.py` (SDK plus instrumentation libraries, called at startup, or in gunicorn's `post_fork`), not `opentelemetry-instrument`: the logging config replaces root handlers, and gunicorn forks after the agent starts. Add manual spans for each pipeline step.
- Log JSON to stdout with `trace_id` and `span_id`; when telemetry is on, the same records also go over OTLP.
- Read configuration from environment variables through typed settings, for example `pydantic-settings`.
- Every outbound call (AI service, S3, other services) has a timeout. Celery retries use exponential backoff with jitter and a maximum number of attempts.
