# syntax=docker/dockerfile:1
# The build context is the repository root, so the build can install the generated contracts in
# proto/gen/python (pyproject.toml points at ../../proto/gen/python). Dockerfile.dockerignore,
# next to this file, sends only those two folders to the builder.
FROM python:3.14.8-slim-trixie
COPY --from=ghcr.io/astral-sh/uv:0.13.1 /uv /bin/uv

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONUNBUFFERED=1 \
    PATH="/src/services/auth/.venv/bin:$PATH"

WORKDIR /src/services/auth
COPY proto/gen/python /src/proto/gen/python
COPY services/auth/pyproject.toml services/auth/uv.lock services/auth/.python-version ./
# --no-editable installs the contracts into the venv, so the image doesn't depend on the path.
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-install-project --no-editable
COPY services/auth/alembic.ini ./
COPY services/auth/migrations ./migrations
COPY services/auth/app ./app

RUN useradd --system --uid 10001 app
USER app
EXPOSE 8080
# The web server; the auth-relay service runs `python -m app.relay` from the same image.
CMD ["python", "-m", "app"]
