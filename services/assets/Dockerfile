# syntax=docker/dockerfile:1
FROM python:3.14.8-slim-trixie
COPY --from=ghcr.io/astral-sh/uv:0.13.1 /uv /bin/uv

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PYTHONUNBUFFERED=1 \
    PATH="/srv/.venv/bin:$PATH"

WORKDIR /srv
COPY pyproject.toml uv.lock .python-version ./
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-install-project
COPY manage.py gunicorn.conf.py ./
COPY config ./config

RUN useradd --system --uid 10001 app
USER app
EXPOSE 8080
CMD ["gunicorn", "config.wsgi:application", "--config", "gunicorn.conf.py"]
