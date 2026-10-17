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
    PATH="/src/services/assets/.venv/bin:$PATH"

WORKDIR /src/services/assets
COPY proto/gen/python /src/proto/gen/python
COPY services/assets/pyproject.toml services/assets/uv.lock services/assets/.python-version ./
# --no-editable installs the contracts into the venv, so the image doesn't depend on the path.
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-install-project --no-editable
COPY services/assets/manage.py services/assets/gunicorn.conf.py ./
COPY services/assets/config ./config
COPY services/assets/drawings ./drawings
COPY services/assets/sso ./sso
# The admin's CSS and JS, served by WhiteNoise (settings.STATIC_ROOT). Settings need a secret
# key to load; this one only exists during the build.
RUN DJANGO_SECRET_KEY=collectstatic-only python manage.py collectstatic --noinput --verbosity 0

# gunicorn keeps its control socket in the home directory, so the user needs one.
RUN useradd --system --create-home --uid 10001 app
USER app
EXPOSE 8080
CMD ["gunicorn", "config.wsgi:application", "--config", "gunicorn.conf.py"]
