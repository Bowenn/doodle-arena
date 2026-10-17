"""Django settings, read from environment variables through typed settings.

The service has two processes from one image: gunicorn serves the REST API and the Django admin
(the moderation console), and a Celery worker runs the drawing pipeline (design doc 4.5).
"""

import socket
from pathlib import Path

from pydantic_settings import BaseSettings

from config import SERVICE_NAME, telemetry
from config.logs import logging_config

BASE_DIR = Path(__file__).resolve().parent.parent


class Env(BaseSettings):
    service_version: str = "dev"
    service_instance_id: str = socket.gethostname()
    log_level: str = "info"
    django_secret_key: str
    db_host: str = "postgres"
    db_port: int = 5432
    db_name: str = "assets"
    db_user: str = "assets"
    db_password: str = ""
    # Access tokens are verified against auth's JWKS (ADR 0001), fetched on the internal network.
    auth_jwks_url: str = "http://auth:8080/api/auth/.well-known/jwks.json"
    jwt_issuer: str = "http://localhost:8080/api/auth"
    # The Celery broker: assets' own logical Redis database (design doc, section 5).
    celery_broker_url: str = "redis://redis:6379/1"
    # Kafka: assets consumes game.rounds.v1 (design doc 6.2).
    kafka_brokers: str = "kafka:9092"
    # Object storage: SeaweedFS locally, S3 in AWS. The stroke logs are the game server's bucket
    # (read only here); drawings is assets' own.
    s3_endpoint: str = "http://seaweedfs:8333"
    s3_region: str = "us-east-1"
    s3_access_key: str = ""
    s3_secret_key: str = ""
    round_logs_bucket: str = "round-logs"
    drawings_bucket: str = "drawings"
    # MongoDB holds the replays (assets.replays); the user only has access to the assets database.
    mongo_url: str = "mongodb://mongo:27017/assets"
    # The origin browsers see (the gateway). Nginx forwards Host without the port, so Django's
    # CSRF Origin check needs to be told which origin is ours.
    public_base_url: str = "http://localhost:8080"
    # Set in AWS, where the gateway terminates TLS. Locally everything is plain HTTP.
    secure_cookies: bool = False


env = Env()  # required fields (DJANGO_SECRET_KEY) come from the environment

SECRET_KEY = env.django_secret_key
DEBUG = False
# Requests only arrive through the gateway; AWS restricts this to the real host name (ADR 0003).
ALLOWED_HOSTS = ["*"]

INSTALLED_APPS = [
    "django.contrib.admin",
    "django.contrib.auth",
    "django.contrib.contenttypes",
    "django.contrib.sessions",
    "django.contrib.messages",
    "django.contrib.staticfiles",
    "django.contrib.postgres",
    "drawings",
    "sso",
]
MIDDLEWARE = [
    "django.middleware.security.SecurityMiddleware",
    "whitenoise.middleware.WhiteNoiseMiddleware",
    "django.contrib.sessions.middleware.SessionMiddleware",
    "django.middleware.common.CommonMiddleware",
    "django.middleware.csrf.CsrfViewMiddleware",
    "django.contrib.auth.middleware.AuthenticationMiddleware",
    "django.contrib.messages.middleware.MessageMiddleware",
    "django.middleware.clickjacking.XFrameOptionsMiddleware",
]
ROOT_URLCONF = "config.urls"
WSGI_APPLICATION = "config.wsgi.application"

TEMPLATES = [
    {
        "BACKEND": "django.template.backends.django.DjangoTemplates",
        "APP_DIRS": True,
        "OPTIONS": {
            "context_processors": [
                "django.template.context_processors.request",
                "django.contrib.auth.context_processors.auth",
                "django.contrib.messages.context_processors.messages",
            ],
        },
    }
]

# Django connects lazily, so the service starts even before the database is up.
DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "HOST": env.db_host,
        "PORT": env.db_port,
        "NAME": env.db_name,
        "USER": env.db_user,
        "PASSWORD": env.db_password,
        "CONN_MAX_AGE": 60,
    }
}
DEFAULT_AUTO_FIELD = "django.db.models.BigAutoField"

USE_TZ = True
TIME_ZONE = "UTC"

# The admin's CSS and JS, collected into the image at build time and served by WhiteNoise. They
# live under /admin/ so the gateway's /admin/ route covers them.
STATIC_URL = "/admin/static/"
STATIC_ROOT: Path | None = BASE_DIR / "static"

# Moderators sign in with Google through the web app, which hands its access token to
# /admin/sso/ once (ADR 0001). There are no Django passwords. The session is scoped to /admin/
# and expires an hour after sign-in (sso.views sets an absolute expiry), so a revoked role
# stops working within the hour.
SESSION_COOKIE_PATH = "/admin/"
SESSION_COOKIE_AGE = 3600
SESSION_COOKIE_HTTPONLY = True
SESSION_COOKIE_SAMESITE = "Lax"
SESSION_COOKIE_SECURE = env.secure_cookies
CSRF_COOKIE_PATH = "/admin/"
CSRF_COOKIE_SAMESITE = "Lax"
CSRF_COOKIE_SECURE = env.secure_cookies
CSRF_TRUSTED_ORIGINS = [env.public_base_url]
# Admin messages ("2 drawings approved") stay in the session rather than in a site-wide cookie.
MESSAGE_STORAGE = "django.contrib.messages.storage.session.SessionStorage"
# Where admin's own login page sends the browser: the web app's sign-in handoff.
ADMIN_SIGN_IN_URL = "/moderate"

# Celery (config/celery.py reads every CELERY_* setting). Delivery is at least once: a task is
# acknowledged after it finishes, and one whose worker died is redelivered, so every task must
# be idempotent. A drawing's state lives in Postgres, so there is no result backend.
CELERY_BROKER_URL = env.celery_broker_url
CELERY_BROKER_CONNECTION_RETRY_ON_STARTUP = True
# Unacknowledged messages come back after this long. It must exceed the longest task and the
# longest retry countdown, or the Redis transport delivers the task twice.
CELERY_BROKER_TRANSPORT_OPTIONS = {"visibility_timeout": 3600}
CELERY_TASK_DEFAULT_QUEUE = "assets"
CELERY_TASK_ACKS_LATE = True
CELERY_TASK_REJECT_ON_WORKER_LOST = True
CELERY_TASK_IGNORE_RESULT = True
CELERY_WORKER_PREFETCH_MULTIPLIER = 1
# The worker keeps the JSON logging config below instead of installing its own handlers.
CELERY_WORKER_HIJACK_ROOT_LOGGER = False
CELERY_WORKER_REDIRECT_STDOUTS = False
# Celery beat (one process, assets-beat) runs the recovery sweeper (drawings/tasks.py).
CELERY_BEAT_SCHEDULE = {
    "requeue-stale-drawings": {"task": "assets.requeue_stale_drawings", "schedule": 60.0},
}

KAFKA_BROKERS = env.kafka_brokers
S3_ENDPOINT = env.s3_endpoint
S3_REGION = env.s3_region
S3_ACCESS_KEY = env.s3_access_key
S3_SECRET_KEY = env.s3_secret_key
ROUND_LOGS_BUCKET = env.round_logs_bucket
DRAWINGS_BUCKET = env.drawings_bucket
MONGO_URL = env.mongo_url

AUTH_JWKS_URL = env.auth_jwks_url
JWT_ISSUER = env.jwt_issuer

LOGGING = logging_config(SERVICE_NAME, env.service_version, env.log_level, telemetry.enabled())
